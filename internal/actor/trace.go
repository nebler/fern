package actor

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nebler/fern/internal/store"
)

// startSpan starts span name of environment id under ctx's span.
func startSpan(ctx context.Context, tr trace.Tracer, name, id string) (context.Context, trace.Span) {
	return tr.Start(ctx, name, trace.WithAttributes(attribute.String("fern.env", id)))
}

// endSpan ends span, marking it failed if err isn't nil.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// tracedStore is st with a span for every call, so that a trace shows the
// disk's share of a change.
type tracedStore struct {
	st Store
	tr trace.Tracer
}

func (t tracedStore) Create(ctx context.Context, id string, opts store.CreateOptions) (store.Env, error) {
	ctx, span := startSpan(ctx, t.tr, "store.create", id)
	env, err := t.st.Create(ctx, id, opts)
	endSpan(span, err)
	return env, err
}

func (t tracedStore) Get(ctx context.Context, id string) (store.Env, error) {
	ctx, span := startSpan(ctx, t.tr, "store.get", id)
	env, err := t.st.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		// An answer, not a failure: a create looks first.
		span.SetAttributes(attribute.Bool("fern.found", false))
		span.End()
		return env, err
	}
	endSpan(span, err)
	return env, err
}

func (t tracedStore) List(ctx context.Context) ([]store.Env, error) {
	ctx, span := t.tr.Start(ctx, "store.list")
	envs, err := t.st.List(ctx)
	endSpan(span, err)
	return envs, err
}

func (t tracedStore) Delete(ctx context.Context, id string) error {
	ctx, span := startSpan(ctx, t.tr, "store.delete", id)
	err := t.st.Delete(ctx, id)
	endSpan(span, err)
	return err
}

func (t tracedStore) Snapshot(ctx context.Context, id, snapID string) error {
	ctx, span := startSpan(ctx, t.tr, "store.snapshot", id)
	err := t.st.Snapshot(ctx, id, snapID)
	endSpan(span, err)
	return err
}

func (t tracedStore) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	ctx, span := startSpan(ctx, t.tr, "store.delete_snapshot", id)
	err := t.st.DeleteSnapshot(ctx, id, snapID)
	endSpan(span, err)
	return err
}

func (t tracedStore) Snapshots(ctx context.Context, id string) ([]store.Snapshot, error) {
	ctx, span := startSpan(ctx, t.tr, "store.snapshots", id)
	snaps, err := t.st.Snapshots(ctx, id)
	endSpan(span, err)
	return snaps, err
}

func (t tracedStore) Restore(ctx context.Context, id, snapID string) error {
	ctx, span := startSpan(ctx, t.tr, "store.restore", id)
	err := t.st.Restore(ctx, id, snapID)
	endSpan(span, err)
	return err
}

func (t tracedStore) Prune(ctx context.Context, id string) error {
	ctx, span := startSpan(ctx, t.tr, "store.prune", id)
	err := t.st.Prune(ctx, id)
	endSpan(span, err)
	return err
}

func (t tracedStore) Orphans(ctx context.Context) ([]string, error) {
	ctx, span := t.tr.Start(ctx, "store.orphans")
	ids, err := t.st.Orphans(ctx)
	endSpan(span, err)
	return ids, err
}

func (t tracedStore) BuildSource(ctx context.Context, id string, src store.SnapshotRef) error {
	ctx, span := startSpan(ctx, t.tr, "store.build_source", id)
	err := t.st.BuildSource(ctx, id, src)
	endSpan(span, err)
	return err
}

func (t tracedStore) StripSource(ctx context.Context, id string) error {
	ctx, span := startSpan(ctx, t.tr, "store.strip_source", id)
	err := t.st.StripSource(ctx, id)
	endSpan(span, err)
	return err
}

func (t tracedStore) Sources(ctx context.Context, name string) ([]store.SnapshotRef, error) {
	ctx, span := t.tr.Start(ctx, "store.sources", trace.WithAttributes(attribute.String("fern.golden", name)))
	refs, err := t.st.Sources(ctx, name)
	endSpan(span, err)
	return refs, err
}
