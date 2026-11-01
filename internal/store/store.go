// Package store keeps environments and their snapshots as btrfs subvolumes in
// one pool, with a SQLite index beside them. The disk is the truth: after a
// crash, Reconcile brings the index and the disk together.
//
// Layout under the pool root:
//
//	envs/<id>/live/            the workspace of an environment created without a path
//	envs/<id>/snaps/<snap>/    read-only snapshots of the workspace
//	envs/<id>/claude/          Claude's state (CLAUDE_CONFIG_DIR): never snapshotted, so a restore keeps it
//	envs/<id>/snaps/pre-restore-<time>/  the workspace a restore replaced, for a day
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
	"strings"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/nebler/fern/internal/store/index"
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
	// ErrInvalidID means an ID can't be used as a file name or a box's hostname.
	ErrInvalidID = errors.New("invalid id")
	// ErrInvalidPath means a workspace path is outside worktrees/, isn't two
	// levels below it, goes through a symlink, or is taken.
	ErrInvalidPath = errors.New("invalid workspace path")
	// errUnindexed means an environment directory holds data the index has
	// lost; only a person should throw it away.
	errUnindexed = errors.New("exists but isn't in the index")
)

var (
	// Environment IDs are boxes' hostnames, so they are DNS labels.
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
	// From, for a workspace at a path that is a plain directory, is the
	// golden snapshot it is adopted onto: the directory's files over the
	// golden's build output (architecture §7.1).
	From *SnapshotRef
}

// SnapshotRef names snapshot Snapshot of environment Env.
type SnapshotRef struct {
	Env, Snapshot string
}

// CheckEnvID returns an error wrapping ErrInvalidID if id can't name an
// environment.
func CheckEnvID(id string) error {
	if !envIDPattern.MatchString(id) {
		return fmt.Errorf("environment %q: %w", id, ErrInvalidID)
	}
	return nil
}

// agentSuffix names an environment's agent box, the box Claude runs in when
// it has one: an environment ID can't contain a dot, so the names never meet.
const agentSuffix = ".agent"

// AgentBox is the name of environment id's agent box.
func AgentBox(id string) string { return id + agentSuffix }

// IsAgentBox says whether box is an environment's agent box.
func IsAgentBox(box string) bool { return strings.HasSuffix(box, agentSuffix) }

// EnvOfBox is the environment box belongs to: box, or the environment whose
// agent box it is.
func EnvOfBox(box string) string { return strings.TrimSuffix(box, agentSuffix) }

func checkSnapID(id string) error {
	if !snapIDPattern.MatchString(id) {
		return fmt.Errorf("snapshot %q: %w", id, ErrInvalidID)
	}
	return nil
}

// Store is a btrfs pool of environments.
type Store struct {
	root string
	db   *sql.DB
	q    *index.Queries // the index's queries, on db
	// failAt, if set, is called at every step of a change that has more
	// than one; an error stops the change there, as a crash would. Crash
	// tests set it.
	failAt func(step string) error
	// Retention (retention.go); tests change it.
	keepTurns    int
	minFreeBytes uint64
}

// step returns failAt's error for the named step, if failAt is set.
func (s *Store) step(name string) error {
	if s.failAt == nil {
		return nil
	}
	return s.failAt(name)
}

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
	s := &Store{root: root, keepTurns: keepTurns, minFreeBytes: minFreeBytes}
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
	if _, err := db.ExecContext(ctx, index.Schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create index: %w", err)
	}
	s.db, s.q = db, index.New(db)
	return s, nil
}

// Close closes the index.
func (s *Store) Close() error { return s.db.Close() }

// Get returns environment id.
func (s *Store) Get(ctx context.Context, id string) (Env, error) {
	if err := CheckEnvID(id); err != nil {
		return Env{}, err
	}
	e, err := s.q.Env(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Env{}, fmt.Errorf("environment %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Env{}, fmt.Errorf("get %s: %w", id, err)
	}
	return s.env(e), nil
}

// List returns all environments, ordered by ID.
func (s *Store) List(ctx context.Context) ([]Env, error) {
	rows, err := s.q.Envs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	envs := make([]Env, len(rows))
	for i, e := range rows {
		envs[i] = s.env(e)
	}
	return envs, nil
}

// FindByPath returns the environment whose workspace is path or holds it.
func (s *Store) FindByPath(ctx context.Context, path string) (Env, error) {
	for p := filepath.Clean(path); p != s.root && p != filepath.Dir(p); p = filepath.Dir(p) {
		e, err := s.q.EnvAt(ctx, p)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Env{}, fmt.Errorf("find %s: %w", path, err)
		}
		return s.env(e), nil
	}
	return Env{}, fmt.Errorf("environment at %s: %w", path, ErrNotFound)
}

// env returns the environment that row e of the index describes.
func (s *Store) env(e index.Environment) Env {
	return Env{
		ID: e.ID, Workspace: e.Workspace, Claude: s.claudePath(e.ID), Git: e.Git,
		Created: time.Unix(e.CreatedAt, 0).UTC(), BoxToken: e.BoxToken,
	}
}

// now is a creation time as the index keeps it, to the second.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func (s *Store) worktrees() string            { return filepath.Join(s.root, "worktrees") }
func (s *Store) repos() string                { return filepath.Join(s.root, "repos") }
func (s *Store) projects() string             { return filepath.Join(s.root, "projects") }
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

// SnapshotPath returns where environment id's snapshot snapID is: a
// read-only subvolume, which btrfs send can send.
func (s *Store) SnapshotPath(id, snapID string) string { return s.snapPath(id, snapID) }

// BackupIndex writes a consistent copy of the index to path, which must not
// exist, while the daemon runs.
func (s *Store) BackupIndex(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("back up the index: %w", err)
	}
	return nil
}
