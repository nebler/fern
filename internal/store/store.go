// Package store keeps environments and their snapshots as btrfs subvolumes in
// one pool, with a SQLite index beside them. The disk is the truth: after a
// crash, Reconcile brings the index and the disk together.
//
// Layout under the pool root:
//
//	envs/<id>/live/            the workspace of an environment created without a path
//	envs/<id>/snaps/<snap>/    read-only snapshots of the workspace
//	envs/<id>/claude/          Claude's state (CLAUDE_CONFIG_DIR): never snapshotted, so a restore keeps it
//	envs/<id>/live.new/        a restore's copy before the swap, the old workspace after
//	envs/<id>/adopt.new/       an adopt's copy before the swap, the old directory after
//	envs/<id>/incomplete       marks a create or delete that hasn't finished
//	worktrees/<repo>/<name>/   workspaces at a path the caller chose (T3's worktrees)
//	repos/                     the main repositories those worktrees belong to (T3 keeps them)
//	state/fern.sqlite          the index; it holds the box tokens
//
// The workspace, Claude's folder and the main repository's git folder are
// mounted into the environment's box at the same paths, so the caller, the
// daemon and the box all name files the same way.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	// errUnindexed means an environment directory holds data the index has
	// lost; only a person should throw it away.
	errUnindexed = errors.New("exists but isn't in the index")
)

var (
	// Environment IDs name Kubernetes objects, so they are DNS labels.
	envIDPattern  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	snapIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// A worktree's repo and name, as T3 makes them.
	pathPartPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
)

// Env is an environment: a workspace subvolume, its snapshots, and the
// folders its box mounts.
type Env struct {
	ID        string
	Workspace string // on the host and in the box
	Claude    string // Claude's folder, outside the workspace so a restore doesn't rewind it
	// Git is the git folder of the main repository the workspace is a
	// worktree of, as it was at create; empty if it isn't a worktree of one
	// under repos/. The box gets it read-only, so git works there and can't
	// change the repository's config or hooks.
	Git      string
	Created  time.Time
	BoxToken string // authenticates the daemon to the environment's box
}

// Snapshot is a read-only copy of an environment's workspace at one moment.
type Snapshot struct {
	ID      string
	Created time.Time
}

// CreateOptions are the optional parts of a new environment.
type CreateOptions struct {
	Workspace string    // under worktrees/; empty means envs/<id>/live
	Seed      io.Reader // a tar stream to fill the workspace with
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
	workspace  TEXT NOT NULL UNIQUE,
	git        TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	box_token  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS snapshots (
	env        TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
	id         TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (env, id)
);`

// envColumns are the environments columns scanEnv reads, in its order.
const envColumns = `id, workspace, git, created_at, box_token`

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
	s := &Store{root: root}
	if err := os.MkdirAll(filepath.Join(root, "envs"), 0o755); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	// T3 makes its worktrees here, as the box user.
	if err := os.MkdirAll(s.worktrees(), 0o755); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := os.Chown(s.worktrees(), BoxUID, BoxGID); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	// Only root may read the index: it holds every box token.
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	dsn := "file:" + filepath.Join(state, "fern.sqlite") +
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
	s.db = db
	return s, nil
}

// Close closes the index.
func (s *Store) Close() error { return s.db.Close() }

// Get returns environment id.
func (s *Store) Get(ctx context.Context, id string) (Env, error) {
	if err := CheckEnvID(id); err != nil {
		return Env{}, err
	}
	env, err := s.scanEnv(s.db.QueryRowContext(ctx, `SELECT `+envColumns+` FROM environments WHERE id = ?`, id))
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+envColumns+` FROM environments ORDER BY id`)
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

// FindByPath returns the environment whose workspace is path or holds it.
func (s *Store) FindByPath(ctx context.Context, path string) (Env, error) {
	for p := filepath.Clean(path); p != s.root && p != filepath.Dir(p); p = filepath.Dir(p) {
		env, err := s.scanEnv(s.db.QueryRowContext(ctx, `SELECT `+envColumns+` FROM environments WHERE workspace = ?`, p))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Env{}, fmt.Errorf("find %s: %w", path, err)
		}
		return env, nil
	}
	return Env{}, fmt.Errorf("environment at %s: %w", path, ErrNotFound)
}

func (s *Store) scanEnv(row interface{ Scan(...any) error }) (Env, error) {
	var env Env
	var created int64
	if err := row.Scan(&env.ID, &env.Workspace, &env.Git, &created, &env.BoxToken); err != nil {
		return Env{}, err
	}
	env.Created = time.Unix(created, 0).UTC()
	env.Claude = s.claudePath(env.ID)
	return env, nil
}

// now is a creation time as the index keeps it, to the second.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func (s *Store) worktrees() string            { return filepath.Join(s.root, "worktrees") }
func (s *Store) repos() string                { return filepath.Join(s.root, "repos") }
func (s *Store) envDir(id string) string      { return filepath.Join(s.root, "envs", id) }
func (s *Store) livePath(id string) string    { return filepath.Join(s.envDir(id), "live") }
func (s *Store) claudePath(id string) string  { return filepath.Join(s.envDir(id), "claude") }
func (s *Store) restorePath(id string) string { return filepath.Join(s.envDir(id), "live.new") }
func (s *Store) adoptPath(id string) string   { return filepath.Join(s.envDir(id), "adopt.new") }
func (s *Store) markerPath(id string) string  { return filepath.Join(s.envDir(id), "incomplete") }
func (s *Store) snapsDir(id string) string    { return filepath.Join(s.envDir(id), "snaps") }

func (s *Store) snapPath(id, snapID string) string {
	return filepath.Join(s.snapsDir(id), snapID)
}
