// Package store keeps environments and their snapshots as btrfs subvolumes in
// one pool, with a SQLite index beside them.
//
// Layout under the pool root:
//
//	envs/<env>/live/            the workspace, unless the environment has a path
//	envs/<env>/snaps/<snap>/    read-only snapshots of the workspace
//	envs/<env>/incomplete       marks a create or delete that hasn't finished
//	worktrees/<repo>/<name>/    workspaces at a path the caller chose (T3's worktrees)
//	state/fern.sqlite           the index; it holds the boxes' tokens
//
// The workspace is mounted into the environment's box at the same path, so
// the caller, the daemon and the box all name files the same way.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// BoxUID and BoxGID own every workspace, so the non-root box (and T3, which
// runs as the same user) can write to it.
const (
	BoxUID = 1000
	BoxGID = 1000
)

var (
	// ErrNotFound means the environment or snapshot doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidID means an ID can't be used as a file name or a Kubernetes name.
	ErrInvalidID = errors.New("invalid id")
	// ErrInvalidPath means a workspace path is outside worktrees/, isn't two
	// levels below it, goes through a symlink, or is taken.
	ErrInvalidPath = errors.New("invalid workspace path")
)

var (
	// Environment IDs name Kubernetes objects, so they are DNS labels.
	envIDPattern  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)
	snapIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// A worktree's repo and name, as T3 makes them.
	pathPartPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Env is an environment: a writable workspace subvolume plus its snapshots.
type Env struct {
	ID      string
	Repo    string
	Path    string // the workspace, on the host and in the box
	Head    string // last snapshot taken or restored
	Created time.Time
	// Token authenticates the daemon to the environment's box.
	Token string
}

// Snapshot is a read-only copy of an environment's workspace at one moment.
type Snapshot struct {
	ID      string
	Env     string
	Created time.Time
}

// CreateOptions are the optional parts of a new environment.
type CreateOptions struct {
	Repo string    // where the code came from, for listings
	Path string    // the workspace, under worktrees/; empty means envs/<id>/live
	Seed io.Reader // a tar stream to fill the workspace with
}

// CheckEnvID returns an error wrapping ErrInvalidID if id can't name an
// environment.
func CheckEnvID(id string) error {
	if !envIDPattern.MatchString(id) {
		return fmt.Errorf("environment %q: %w", id, ErrInvalidID)
	}
	return nil
}

// Store is a btrfs pool of environments.
type Store struct {
	root string
	db   *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS environments (
	id         TEXT PRIMARY KEY,
	repo       TEXT NOT NULL,
	head       TEXT NOT NULL DEFAULT '',
	token      TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	path       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS snapshots (
	env        TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
	id         TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (env, id)
);`

// pathIndex makes a workspace path belong to one environment. It is created
// after the migration that adds the column.
const pathIndex = `CREATE UNIQUE INDEX IF NOT EXISTS environments_path ON environments(path) WHERE path != ''`

// Open opens the pool mounted at root, which must be on btrfs, creating the
// index if needed.
func Open(ctx context.Context, root string) (*Store, error) {
	var fsInfo unix.Statfs_t
	if err := unix.Statfs(root, &fsInfo); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if fsInfo.Type != unix.BTRFS_SUPER_MAGIC {
		return nil, fmt.Errorf("open pool: %s is not on btrfs", root)
	}
	if err := os.MkdirAll(filepath.Join(root, "envs"), 0o755); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	// T3 makes its worktrees here, as the box user.
	worktrees := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktrees, 0o755); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := os.Chown(worktrees, BoxUID, BoxGID); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	// Only root may read the index: it holds every box's token.
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	dsn := "file:" + filepath.Join(root, "state", "fern.sqlite") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create index: %w", err)
	}
	return &Store{root: root, db: db}, nil
}

// migrate creates the index, or brings an older one up to date.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	var hasPath bool
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) > 0 FROM pragma_table_info('environments') WHERE name = 'path'`).Scan(&hasPath); err != nil {
		return err
	}
	if !hasPath {
		if _, err := db.ExecContext(ctx, `ALTER TABLE environments ADD COLUMN path TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx, pathIndex)
	return err
}

// Close closes the index.
func (s *Store) Close() error { return s.db.Close() }

// Create makes an environment, its workspace filled from opts.Seed if it
// isn't nil. Creating an environment that exists returns it unchanged.
//
// With opts.Path, the workspace is that path: if nothing is there yet, an
// empty subvolume (about 10 ms; `git worktree add` can fill it); if a plain
// directory is there, it is adopted, which costs one reflink per file.
func (s *Store) Create(ctx context.Context, id string, opts CreateOptions) (Env, error) {
	env, err := s.Get(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		if err == nil && opts.Path != "" && filepath.Clean(opts.Path) != env.Path {
			return Env{}, fmt.Errorf("create %s: it exists with workspace %s: %w", id, env.Path, ErrInvalidPath)
		}
		return env, err
	}
	path := s.livePath(id)
	if opts.Path != "" {
		if path, err = s.checkPath(ctx, opts.Path); err != nil {
			return Env{}, fmt.Errorf("create %s: %w", id, err)
		}
	}
	if err := s.clearLeftovers(ctx, id); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if err := os.MkdirAll(filepath.Join(s.envDir(id), "snaps"), 0o755); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if err := s.mark(id, opCreate, path); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if err := s.makeWorkspace(ctx, id, path); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if opts.Seed != nil {
		if err := untar(ctx, path, opts.Seed); err != nil {
			return Env{}, fmt.Errorf("create %s: %w", id, err)
		}
		// The row must not outlive a power cut that the files don't.
		if err := syncfs(path); err != nil {
			return Env{}, fmt.Errorf("create %s: %w", id, err)
		}
	}
	env = Env{ID: id, Repo: opts.Repo, Path: path, Token: rand.Text(), Created: now()}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO environments (id, repo, token, created_at, path) VALUES (?, ?, ?, ?, ?)`,
		env.ID, env.Repo, env.Token, env.Created.Unix(), env.Path)
	if err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	// With the row written, a leftover marker means nothing.
	if err := os.Remove(s.markerPath(id)); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	return env, nil
}

// checkPath checks that path can be a new workspace and returns it cleaned.
func (s *Store) checkPath(ctx context.Context, path string) (string, error) {
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(s.worktrees(), clean)
	parts := strings.Split(rel, string(filepath.Separator))
	if err != nil || len(parts) != 2 || !pathPartPattern.MatchString(parts[0]) || !pathPartPattern.MatchString(parts[1]) {
		return "", fmt.Errorf("%s: want %s/<repo>/<name>: %w", path, s.worktrees(), ErrInvalidPath)
	}
	// What the box mounts must be what the caller named.
	if fi, err := os.Lstat(clean); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink: %w", path, ErrInvalidPath)
	}
	if fi, err := os.Lstat(filepath.Dir(clean)); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink: %w", filepath.Dir(clean), ErrInvalidPath)
	}
	var other string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM environments WHERE path = ?`, clean).Scan(&other)
	if err == nil {
		return "", fmt.Errorf("%s is environment %s's workspace: %w", path, other, ErrInvalidPath)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return clean, nil
}

// makeWorkspace makes path a subvolume owned by the box user, keeping what
// is already there.
func (s *Store) makeWorkspace(ctx context.Context, id, path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		if err := os.Chown(parent, BoxUID, BoxGID); err != nil {
			return err
		}
		if err := btrfs(ctx, "subvolume", "create", path); err != nil {
			return err
		}
		return os.Chown(path, BoxUID, BoxGID)
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory: %w", path, ErrInvalidPath)
	case isSubvolume(fi):
		// A create that stopped after making it, or one that had already
		// adopted it.
		return nil
	default:
		return s.adopt(ctx, id, path)
	}
}

// adopt turns the plain directory path into a subvolume with the same files:
// a reflink copy (no data is copied, but every file gets a new inode) that is
// swapped in with one atomic rename. Writes to path during the copy are lost,
// so adopt before an agent starts working in it.
func (s *Store) adopt(ctx context.Context, id, path string) error {
	next := filepath.Join(s.envDir(id), "adopt.new")
	if err := deleteSubvolumes(ctx, next); err != nil {
		return err
	}
	if err := os.RemoveAll(next); err != nil { // the old directory of an adopt that stopped
		return err
	}
	if err := btrfs(ctx, "subvolume", "create", next); err != nil {
		return err
	}
	// -T copies path's own owner and mode onto next, and its contents into it.
	out, err := exec.CommandContext(ctx, "cp", "-a", "--reflink=always", "-T", path, next).CombinedOutput()
	if err != nil {
		return fmt.Errorf("adopt %s: cp: %w: %s", path, err, bytes.TrimSpace(out))
	}
	if err := unix.Renameat2(unix.AT_FDCWD, next, unix.AT_FDCWD, path, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("adopt %s: swap: %w", path, err)
	}
	// next is now the old plain directory.
	return os.RemoveAll(next)
}

// Get returns the environment id.
func (s *Store) Get(ctx context.Context, id string) (Env, error) {
	if err := CheckEnvID(id); err != nil {
		return Env{}, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repo, head, token, created_at, path FROM environments WHERE id = ?`, id)
	env, err := s.scanEnv(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Env{}, fmt.Errorf("environment %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Env{}, fmt.Errorf("get %s: %w", id, err)
	}
	return env, nil
}

// List returns all environments, ordered by ID.
func (s *Store) List(ctx context.Context) ([]Env, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo, head, token, created_at, path FROM environments ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var envs []Env
	for rows.Next() {
		env, err := s.scanEnv(rows)
		if err != nil {
			return nil, fmt.Errorf("list environments: %w", err)
		}
		envs = append(envs, env)
	}
	return envs, rows.Err()
}

// LivePath returns the path of environment id's workspace.
func (s *Store) LivePath(ctx context.Context, id string) (string, error) {
	env, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return env.Path, nil
}

// FindByPath returns the environment whose workspace is path or holds it.
func (s *Store) FindByPath(ctx context.Context, path string) (Env, error) {
	for p := filepath.Clean(path); p != s.root && p != filepath.Dir(p); p = filepath.Dir(p) {
		var id string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM environments WHERE path = ?`, p).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Env{}, fmt.Errorf("find %s: %w", path, err)
		}
		return s.Get(ctx, id)
	}
	return Env{}, fmt.Errorf("environment at %s: %w", path, ErrNotFound)
}

// Delete removes the environment's index rows and subvolumes, its workspace
// included. Deleting an environment that doesn't exist is not an error. Its
// box must be gone.
//
// The rows go first, so an environment is never listed without its
// workspace. A delete that stops half-way leaves marked garbage, which
// Reconcile or the next Create of the same ID removes.
func (s *Store) Delete(ctx context.Context, id string) error {
	env, err := s.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		if err := s.clearLeftovers(ctx, id); err != nil {
			return fmt.Errorf("delete %s: %w", id, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.mark(id, opDelete, env.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	if err := s.removeDisk(ctx, id, env.Path); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	return nil
}

// Snapshot freezes environment envID's workspace as the read-only snapshot
// snapID. Taking a snapshot that exists returns it unchanged, so callers can
// retry with the same ID; DeleteSnapshot first to take it again.
func (s *Store) Snapshot(ctx context.Context, envID, snapID string) (Snapshot, error) {
	if !snapIDPattern.MatchString(snapID) {
		return Snapshot{}, fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	env, err := s.Get(ctx, envID)
	if err != nil {
		return Snapshot{}, err
	}
	path := s.snapPath(envID, snapID)
	snap, err := s.snapshot(ctx, envID, snapID)
	switch {
	case err == nil:
		_, statErr := os.Lstat(path)
		if !errors.Is(statErr, fs.ErrNotExist) {
			return snap, statErr
		}
		// The row outlived its subvolume (a DeleteSnapshot that stopped
		// half-way, whose caller wanted it gone): take it again.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE env = ? AND id = ?`, envID, snapID); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot %s/%s: %w", envID, snapID, err)
		}
	case !errors.Is(err, ErrNotFound):
		return Snapshot{}, err
	}
	// A snapshot on disk without a row was taken just before a crash. btrfs
	// snapshots are atomic, so it is complete: keep it. Taking one flushes
	// the workspace's dirty pages first, so it holds every write made before.
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := btrfs(ctx, "subvolume", "snapshot", "-r", env.Path, path); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot %s/%s: %w", envID, snapID, err)
		}
	} else if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s/%s: %w", envID, snapID, err)
	}
	snap = Snapshot{ID: snapID, Env: envID, Created: now()}
	if err := s.recordSnapshot(ctx, snap); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s/%s: %w", envID, snapID, err)
	}
	return snap, nil
}

// DeleteSnapshot removes snapshot snapID of environment envID. Deleting one
// that doesn't exist is not an error. The subvolume goes first, so a
// snapshot on disk without a row is always one that was being taken.
func (s *Store) DeleteSnapshot(ctx context.Context, envID, snapID string) error {
	if !snapIDPattern.MatchString(snapID) {
		return fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	if _, err := s.Get(ctx, envID); err != nil {
		return err
	}
	if err := deleteSubvolumes(ctx, s.snapPath(envID, snapID)); err != nil {
		return fmt.Errorf("delete snapshot %s/%s: %w", envID, snapID, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE env = ? AND id = ?`, envID, snapID); err != nil {
		return fmt.Errorf("delete snapshot %s/%s: %w", envID, snapID, err)
	}
	return nil
}

// Snapshots returns environment envID's snapshots, oldest first.
func (s *Store) Snapshots(ctx context.Context, envID string) ([]Snapshot, error) {
	if _, err := s.Get(ctx, envID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, env, created_at FROM snapshots WHERE env = ? ORDER BY created_at, rowid`, envID)
	if err != nil {
		return nil, fmt.Errorf("list snapshots of %s: %w", envID, err)
	}
	defer func() { _ = rows.Close() }()
	var snaps []Snapshot
	for rows.Next() {
		var snap Snapshot
		var created int64
		if err := rows.Scan(&snap.ID, &snap.Env, &created); err != nil {
			return nil, fmt.Errorf("list snapshots of %s: %w", envID, err)
		}
		snap.Created = time.Unix(created, 0).UTC()
		snaps = append(snaps, snap)
	}
	return snaps, rows.Err()
}

// Restore replaces environment envID's workspace with a writable copy of snapshot
// snapID. The environment's box must be stopped. Every step can be repeated
// after a crash.
func (s *Store) Restore(ctx context.Context, envID, snapID string) error {
	if !snapIDPattern.MatchString(snapID) {
		return fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	env, err := s.Get(ctx, envID)
	if err != nil {
		return err
	}
	if _, err := s.snapshot(ctx, envID, snapID); err != nil {
		return err
	}
	live := env.Path
	next := filepath.Join(s.envDir(envID), "live.new")
	// A leftover live.new is a half-made copy, or the old workspace of a
	// restore that stopped before its last step. Either way it is garbage.
	if err := deleteSubvolumes(ctx, next); err != nil {
		return fmt.Errorf("restore %s to %s: %w", envID, snapID, err)
	}
	if err := btrfs(ctx, "subvolume", "snapshot", s.snapPath(envID, snapID), next); err != nil {
		return fmt.Errorf("restore %s to %s: %w", envID, snapID, err)
	}
	// One atomic step: the workspace is always either the old or the new tree.
	if err := unix.Renameat2(unix.AT_FDCWD, next, unix.AT_FDCWD, live, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("restore %s to %s: swap: %w", envID, snapID, err)
	}
	if err := deleteSubvolumes(ctx, next); err != nil {
		return fmt.Errorf("restore %s to %s: %w", envID, snapID, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE environments SET head = ? WHERE id = ?`, snapID, envID); err != nil {
		return fmt.Errorf("restore %s to %s: %w", envID, snapID, err)
	}
	return nil
}

// snapshot looks up a snapshot of an environment that exists.
func (s *Store) snapshot(ctx context.Context, envID, snapID string) (Snapshot, error) {
	snap := Snapshot{ID: snapID, Env: envID}
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT created_at FROM snapshots WHERE env = ? AND id = ?`, envID, snapID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("snapshot %s/%s: %w", envID, snapID, ErrNotFound)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("get snapshot %s/%s: %w", envID, snapID, err)
	}
	snap.Created = time.Unix(created, 0).UTC()
	return snap, nil
}

func (s *Store) recordSnapshot(ctx context.Context, snap Snapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO snapshots (env, id, created_at) VALUES (?, ?, ?)`,
		snap.Env, snap.ID, snap.Created.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE environments SET head = ? WHERE id = ?`, snap.ID, snap.Env); err != nil {
		return err
	}
	return tx.Commit()
}

// Reconcile brings the disk and the index together after a crash: it finishes
// the creates and deletes that stopped half-way, removes the leftovers of
// restores and adopts, and returns the environment directories it keeps
// because the index has lost them.
func (s *Store) Reconcile(ctx context.Context) (lost []string, err error) {
	dirs, err := os.ReadDir(filepath.Join(s.root, "envs"))
	if err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	for _, d := range dirs {
		id := d.Name()
		if CheckEnvID(id) != nil {
			lost = append(lost, s.envDir(id))
			continue
		}
		_, err := s.Get(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			if err := s.clearLeftovers(ctx, id); err != nil {
				lost = append(lost, s.envDir(id))
			}
		case err != nil:
			return lost, fmt.Errorf("reconcile: %w", err)
		default:
			next := filepath.Join(s.envDir(id), "adopt.new")
			if err := deleteSubvolumes(ctx, filepath.Join(s.envDir(id), "live.new"), next); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := os.RemoveAll(next); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := os.Remove(s.markerPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
		}
	}
	return lost, nil
}

// clearLeftovers removes what a create or delete of environment id (which
// has no index row) left when it stopped half-way. It refuses an unmarked
// directory that holds a workspace or snapshots: then the index has lost
// them, and only a person should throw them away.
func (s *Store) clearLeftovers(ctx context.Context, id string) error {
	dir := s.envDir(id)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	op, path, err := s.readMarker(id)
	if err != nil {
		return err
	}
	if op == "" {
		kept, err := holdsData(dir)
		if err != nil {
			return err
		}
		if kept {
			return fmt.Errorf("%s exists but isn't in the index", dir)
		}
	}
	// A half-made workspace of our own is garbage. A workspace at a caller's
	// path is not: a create that stopped there is resumed by the next Create,
	// which keeps the subvolume it finds; only a delete removes it.
	if op != opDelete && path != s.livePath(id) {
		path = ""
	}
	return s.removeDisk(ctx, id, path)
}

// holdsData reports whether an environment directory has a live/ or any
// snapshot.
func holdsData(dir string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, "live")); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	snaps, err := os.ReadDir(filepath.Join(dir, "snaps"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return len(snaps) > 0, nil
}

// removeDisk deletes environment id's directory and, if workspace isn't
// empty, its workspace. All subvolumes go in one btrfs call.
func (s *Store) removeDisk(ctx context.Context, id, workspace string) error {
	dir := s.envDir(id)
	paths := []string{s.livePath(id), filepath.Join(dir, "live.new"), filepath.Join(dir, "adopt.new")}
	if workspace != "" && workspace != s.livePath(id) {
		paths = append(paths, workspace)
	}
	snaps, err := os.ReadDir(filepath.Join(dir, "snaps"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, snap := range snaps {
		paths = append(paths, s.snapPath(id, snap.Name()))
	}
	if err := deleteSubvolumes(ctx, paths...); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// The operations a marker records.
const (
	opCreate = "create"
	opDelete = "delete"
)

// mark records that a create or delete of environment id, whose workspace is
// path, has begun. It fails with fs.ErrNotExist if the environment has no
// directory.
func (s *Store) mark(id, op, path string) error {
	return os.WriteFile(s.markerPath(id), []byte(op+" "+path+"\n"), 0o644)
}

// readMarker returns what the marker of environment id records, or an empty
// op if it has none.
func (s *Store) readMarker(id string) (op, path string, err error) {
	data, err := os.ReadFile(s.markerPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	op, path, _ = strings.Cut(strings.TrimSpace(string(data)), " ")
	if op != opCreate && op != opDelete {
		// A marker from before markers held anything: a create of live/.
		op, path = opCreate, s.livePath(id)
	}
	return op, path, nil
}

func (s *Store) worktrees() string           { return filepath.Join(s.root, "worktrees") }
func (s *Store) envDir(id string) string     { return filepath.Join(s.root, "envs", id) }
func (s *Store) markerPath(id string) string { return filepath.Join(s.envDir(id), "incomplete") }
func (s *Store) livePath(id string) string   { return filepath.Join(s.envDir(id), "live") }
func (s *Store) snapPath(id, snapID string) string {
	return filepath.Join(s.envDir(id), "snaps", snapID)
}

func (s *Store) scanEnv(row interface{ Scan(...any) error }) (Env, error) {
	var env Env
	var created int64
	if err := row.Scan(&env.ID, &env.Repo, &env.Head, &env.Token, &created, &env.Path); err != nil {
		return Env{}, err
	}
	env.Created = time.Unix(created, 0).UTC()
	if env.Path == "" { // a row from before workspaces had paths
		env.Path = s.livePath(env.ID)
	}
	return env, nil
}

// now is the creation time recorded in the index, at the index's resolution.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func btrfs(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "btrfs", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// deleteSubvolumes deletes those of paths that are subvolumes, with any
// subvolumes a box made inside them, in one btrfs call. Paths that don't
// exist or aren't subvolumes are left alone.
func deleteSubvolumes(ctx context.Context, paths ...string) error {
	args := []string{"subvolume", "delete", "--recursive"}
	n := len(args)
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if isSubvolume(fi) {
			args = append(args, p)
		}
	}
	if len(args) == n {
		return nil
	}
	return btrfs(ctx, args...)
}

// isSubvolume reports whether fi describes the root of a btrfs subvolume,
// which always has inode 256.
func isSubvolume(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.IsDir() && st.Ino == 256
}

// syncfs flushes the filesystem holding dir.
func syncfs(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return unix.Syncfs(int(f.Fd()))
}

// untar extracts a tar stream into dir as the box user, so the files belong to
// it and a hostile archive can't use root's rights.
func untar(ctx context.Context, dir string, r io.Reader) error {
	cmd := exec.CommandContext(ctx, "tar", "-x", "--no-same-owner", "-C", dir, "-f", "-")
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: BoxUID, Gid: BoxGID}}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("untar: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
