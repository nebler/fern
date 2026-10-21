// Package store keeps environments and their snapshots as btrfs subvolumes in
// one pool, with a SQLite index beside them.
//
// Layout under the pool root:
//
//	envs/<env>/live/          writable; mounted into the environment's box
//	envs/<env>/snaps/<snap>/  read-only snapshots of live/
//	envs/<env>/incomplete     marks a create or delete that hasn't finished
//	state/fern.sqlite         the index; it holds the boxes' tokens
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

// BoxUID and BoxGID own every environment's live/ tree, so the non-root box
// can write to it.
const (
	BoxUID = 1000
	BoxGID = 1000
)

var (
	// ErrNotFound means the environment or snapshot doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidID means an ID can't be used as a file name or a Kubernetes name.
	ErrInvalidID = errors.New("invalid id")
)

var (
	// Environment IDs name Kubernetes objects, so they are DNS labels.
	envIDPattern  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)
	snapIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Env is an environment: a writable live/ subvolume plus its snapshots.
type Env struct {
	ID      string
	Repo    string
	Head    string // last snapshot taken or restored
	Created time.Time
	// Token authenticates the daemon to the environment's box.
	Token string
}

// Snapshot is a read-only copy of an environment's live/ at one moment.
type Snapshot struct {
	ID      string
	Env     string
	Created time.Time
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
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS snapshots (
	env        TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
	id         TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (env, id)
);`

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
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create index: %w", err)
	}
	return &Store{root: root, db: db}, nil
}

// Close closes the index.
func (s *Store) Close() error { return s.db.Close() }

// Create makes an environment with an empty live/, filled from seed (a tar
// stream) if seed isn't nil. Creating an environment that exists returns it
// unchanged.
func (s *Store) Create(ctx context.Context, id, repo string, seed io.Reader) (Env, error) {
	env, err := s.Get(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		return env, err
	}
	// Anything on disk without an index row is a create or delete that
	// stopped half-way, which is garbage, unless it has a live/ that isn't
	// marked as one: then the index has lost a workspace, and only a person
	// should throw it away.
	if _, err := os.Stat(s.markerPath(id)); errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Lstat(s.livePath(id)); err == nil {
			return Env{}, fmt.Errorf("create %s: %s exists but isn't in the index", id, s.envDir(id))
		}
	}
	if err := s.removeDisk(ctx, id); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	live := s.livePath(id)
	if err := os.MkdirAll(filepath.Join(s.envDir(id), "snaps"), 0o755); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if err := s.mark(id); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if err := btrfs(ctx, "subvolume", "create", live); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if err := os.Chown(live, BoxUID, BoxGID); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	if seed != nil {
		if err := untar(ctx, live, seed); err != nil {
			return Env{}, fmt.Errorf("create %s: %w", id, err)
		}
	}
	env = Env{ID: id, Repo: repo, Token: rand.Text(), Created: now()}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO environments (id, repo, token, created_at) VALUES (?, ?, ?, ?)`,
		env.ID, env.Repo, env.Token, env.Created.Unix())
	if err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	// With the row written, a leftover marker means nothing.
	if err := os.Remove(s.markerPath(id)); err != nil {
		return Env{}, fmt.Errorf("create %s: %w", id, err)
	}
	return env, nil
}

// Get returns the environment id.
func (s *Store) Get(ctx context.Context, id string) (Env, error) {
	if err := CheckEnvID(id); err != nil {
		return Env{}, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repo, head, token, created_at FROM environments WHERE id = ?`, id)
	env, err := scanEnv(row)
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
		`SELECT id, repo, head, token, created_at FROM environments ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var envs []Env
	for rows.Next() {
		env, err := scanEnv(rows)
		if err != nil {
			return nil, fmt.Errorf("list environments: %w", err)
		}
		envs = append(envs, env)
	}
	return envs, rows.Err()
}

// LivePath returns the path of environment id's live/ subvolume.
func (s *Store) LivePath(ctx context.Context, id string) (string, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return "", err
	}
	return s.livePath(id), nil
}

// Delete removes the environment's index rows and subvolumes. Deleting an
// environment that doesn't exist is not an error. Its box must be gone.
//
// The rows go first, so an environment is never listed without its disk. A
// delete that stops half-way leaves marked garbage, which the next Create of
// the same ID removes.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := CheckEnvID(id); err != nil {
		return err
	}
	if err := s.mark(id); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	if err := s.removeDisk(ctx, id); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	return nil
}

// Snapshot freezes environment envID's live/ as the read-only snapshot
// snapID. Taking a snapshot that exists returns it unchanged, so callers can
// retry with the same ID.
func (s *Store) Snapshot(ctx context.Context, envID, snapID string) (Snapshot, error) {
	if !snapIDPattern.MatchString(snapID) {
		return Snapshot{}, fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	if _, err := s.Get(ctx, envID); err != nil {
		return Snapshot{}, err
	}
	snap, err := s.snapshot(ctx, envID, snapID)
	if !errors.Is(err, ErrNotFound) {
		return snap, err
	}
	path := s.snapPath(envID, snapID)
	// A snapshot on disk without a row was taken just before a crash. btrfs
	// snapshots are atomic, so it is complete: keep it. Taking one flushes
	// live/'s dirty pages first, so it holds every write made before.
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := btrfs(ctx, "subvolume", "snapshot", "-r", s.livePath(envID), path); err != nil {
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

// Restore replaces environment envID's live/ with a writable copy of snapshot
// snapID. The environment's box must be stopped. Every step can be repeated
// after a crash.
func (s *Store) Restore(ctx context.Context, envID, snapID string) error {
	if !snapIDPattern.MatchString(snapID) {
		return fmt.Errorf("snapshot %q: %w", snapID, ErrInvalidID)
	}
	if _, err := s.Get(ctx, envID); err != nil {
		return err
	}
	if _, err := s.snapshot(ctx, envID, snapID); err != nil {
		return err
	}
	live := s.livePath(envID)
	next := live + ".new"
	// A leftover live.new is a half-made copy, or the old live/ of a restore
	// that stopped before its last step. Either way it is garbage.
	if err := deleteSubvolume(ctx, next); err != nil {
		return fmt.Errorf("restore %s to %s: %w", envID, snapID, err)
	}
	if err := btrfs(ctx, "subvolume", "snapshot", s.snapPath(envID, snapID), next); err != nil {
		return fmt.Errorf("restore %s to %s: %w", envID, snapID, err)
	}
	// One atomic step: live/ is always either the old or the new tree.
	if err := unix.Renameat2(unix.AT_FDCWD, next, unix.AT_FDCWD, live, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("restore %s to %s: swap: %w", envID, snapID, err)
	}
	if err := deleteSubvolume(ctx, next); err != nil {
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

// removeDisk deletes everything environment id has on disk.
func (s *Store) removeDisk(ctx context.Context, id string) error {
	dir := s.envDir(id)
	paths := []string{s.livePath(id), s.livePath(id) + ".new"}
	snaps, err := os.ReadDir(filepath.Join(dir, "snaps"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, snap := range snaps {
		paths = append(paths, s.snapPath(id, snap.Name()))
	}
	for _, path := range paths {
		if err := deleteSubvolume(ctx, path); err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

// mark records that a create or delete of environment id has begun. It fails
// with fs.ErrNotExist if the environment has no directory.
func (s *Store) mark(id string) error {
	return os.WriteFile(s.markerPath(id), nil, 0o644)
}

func (s *Store) envDir(id string) string     { return filepath.Join(s.root, "envs", id) }
func (s *Store) markerPath(id string) string { return filepath.Join(s.envDir(id), "incomplete") }
func (s *Store) livePath(id string) string   { return filepath.Join(s.envDir(id), "live") }
func (s *Store) snapPath(id, snapID string) string {
	return filepath.Join(s.envDir(id), "snaps", snapID)
}

func scanEnv(row interface{ Scan(...any) error }) (Env, error) {
	var env Env
	var created int64
	if err := row.Scan(&env.ID, &env.Repo, &env.Head, &env.Token, &created); err != nil {
		return Env{}, err
	}
	env.Created = time.Unix(created, 0).UTC()
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

// deleteSubvolume deletes the subvolume at path, if there is one, with any
// subvolumes a box made inside it.
func deleteSubvolume(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return btrfs(ctx, "subvolume", "delete", "--recursive", path)
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
