package actor

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/span"
	"github.com/nebler/fern/internal/store"
)

// tracedStore is st with a span for every call, so that a trace shows the
// disk's share of a change.
type tracedStore struct {
	st Store
	tr trace.Tracer
}

func (t tracedStore) Create(ctx context.Context, id string, opts store.CreateOptions) (store.Env, error) {
	ctx, sp := span.Start(ctx, t.tr, "store.create", id)
	env, err := t.st.Create(ctx, id, opts)
	span.End(sp, err)
	return env, err
}

func (t tracedStore) Get(ctx context.Context, id string) (store.Env, error) {
	ctx, sp := span.Start(ctx, t.tr, "store.get", id)
	env, err := t.st.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		// An answer, not a failure: a create looks first.
		sp.SetAttributes(attribute.Bool("fern.found", false))
		sp.End()
		return env, err
	}
	span.End(sp, err)
	return env, err
}

func (t tracedStore) List(ctx context.Context) ([]store.Env, error) {
	ctx, sp := t.tr.Start(ctx, "store.list")
	envs, err := t.st.List(ctx)
	span.End(sp, err)
	return envs, err
}

func (t tracedStore) Delete(ctx context.Context, id string) error {
	ctx, sp := span.Start(ctx, t.tr, "store.delete", id)
	err := t.st.Delete(ctx, id)
	span.End(sp, err)
	return err
}

func (t tracedStore) Snapshot(ctx context.Context, id, snapID string) error {
	ctx, sp := span.Start(ctx, t.tr, "store.snapshot", id)
	err := t.st.Snapshot(ctx, id, snapID)
	span.End(sp, err)
	return err
}

func (t tracedStore) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	ctx, sp := span.Start(ctx, t.tr, "store.delete_snapshot", id)
	err := t.st.DeleteSnapshot(ctx, id, snapID)
	span.End(sp, err)
	return err
}

func (t tracedStore) HasSnapshot(ctx context.Context, ref store.SnapshotRef) (bool, error) {
	ctx, sp := span.Start(ctx, t.tr, "store.has_snapshot", ref.Env)
	ok, err := t.st.HasSnapshot(ctx, ref)
	span.End(sp, err)
	return ok, err
}

func (t tracedStore) Restore(ctx context.Context, id, snapID string) error {
	ctx, sp := span.Start(ctx, t.tr, "store.restore", id)
	err := t.st.Restore(ctx, id, snapID)
	span.End(sp, err)
	return err
}

func (t tracedStore) Prune(ctx context.Context, id string) error {
	ctx, sp := span.Start(ctx, t.tr, "store.prune", id)
	err := t.st.Prune(ctx, id)
	span.End(sp, err)
	return err
}

func (t tracedStore) Orphans(ctx context.Context) ([]string, error) {
	ctx, sp := t.tr.Start(ctx, "store.orphans")
	ids, err := t.st.Orphans(ctx)
	span.End(sp, err)
	return ids, err
}

func (t tracedStore) BuildSource(ctx context.Context, id string, src store.SnapshotRef) error {
	ctx, sp := span.Start(ctx, t.tr, "store.build_source", id)
	err := t.st.BuildSource(ctx, id, src)
	span.End(sp, err)
	return err
}

func (t tracedStore) StripSource(ctx context.Context, id string) error {
	ctx, sp := span.Start(ctx, t.tr, "store.strip_source", id)
	err := t.st.StripSource(ctx, id)
	span.End(sp, err)
	return err
}

func (t tracedStore) Sources(ctx context.Context, name string) ([]store.SnapshotRef, error) {
	ctx, sp := t.tr.Start(ctx, "store.sources", trace.WithAttributes(attribute.String("fern.golden", name)))
	refs, err := t.st.Sources(ctx, name)
	span.End(sp, err)
	return refs, err
}
