package actor

import (
	"context"

	"github.com/nebler/fern/internal/store"
)

// Store keeps environments and snapshots on disk (store.Store). It is the
// small interfaces below, one per job the actors ask of the disk: a method
// the store gains for the actors goes in the one whose job it is.
type Store interface {
	Environments
	Inventory
	Snapshots
	Restorer
	Pruner
	GoldenSources
	BuildFailures
	Disk
}

// Environments makes, reads and deletes one environment's record and
// workspace: changes.go's create and delete, golden.go's builders.
type Environments interface {
	Create(ctx context.Context, id string, opts store.CreateOptions) (store.Env, error)
	Get(ctx context.Context, id string) (store.Env, error)
	Delete(ctx context.Context, id string) error
}

// Inventory lists what the pool holds: the environments, for Fork, PruneAll
// and a golden's builders, and those whose worktree is gone, for sweep.
type Inventory interface {
	List(ctx context.Context) ([]store.Env, error)
	Orphans(ctx context.Context) ([]string, error)
}

// Snapshots takes, deletes and looks for an environment's snapshots: a
// turn's (Snapshot) and a golden's.
type Snapshots interface {
	Snapshot(ctx context.Context, id, snapID string) error
	DeleteSnapshot(ctx context.Context, id, snapID string) error
	HasSnapshot(ctx context.Context, ref store.SnapshotRef) (bool, error)
}

// Restorer puts an environment's workspace back to one of its snapshots.
type Restorer interface {
	Restore(ctx context.Context, id, snapID string) error
}

// Pruner deletes the snapshots past what the pool keeps: an environment's
// oldest turns, and a builder's goldens of other keys.
type Pruner interface {
	Prune(ctx context.Context, id string) error
	PruneGoldens(ctx context.Context, id string) error
}

// GoldenSources keeps a repository's golden sources and makes a builder's
// workspace from one (golden.go).
type GoldenSources interface {
	BuildSource(ctx context.Context, id string, src store.SnapshotRef) error
	StripSource(ctx context.Context, id string) error
	Sources(ctx context.Context, name string) ([]store.SnapshotRef, error)
}

// BuildFailures keeps a builder's last failed golden build with it, for the
// next build to skip and for a thread's notices.
type BuildFailures interface {
	SetBuildFailure(ctx context.Context, id string, f *store.BuildFailure) error
	BuildFailure(ctx context.Context, id string) (store.BuildFailure, bool, error)
}

// Disk tells how full the pool is, for a thread's notices.
type Disk interface {
	Space() (store.Space, error)
	// Quota is env's disk limit and use, if limits are on (store.Quota).
	Quota(ctx context.Context, env store.Env) (store.Quota, error)
}

// SnapshotFailures keeps each environment's last failed snapshot with it,
// on disk (store.Store).
type SnapshotFailures interface {
	SetSnapshotFailure(ctx context.Context, id string, f *store.SnapshotFailure) error
	SnapshotFailure(ctx context.Context, id string) (store.SnapshotFailure, bool, error)
}
