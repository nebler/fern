package backup

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/nebler/fern/internal/store"
)

// blockingStore holds the first List until release is closed, so a backup
// stays under way for as long as a test needs.
type blockingStore struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStore) List(ctx context.Context) ([]store.Env, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil, errors.New("stopped for the test")
}

func (s *blockingStore) Snapshots(context.Context, string) ([]store.Snapshot, error) { return nil, nil }
func (s *blockingStore) SnapshotPath(string, string) string                          { return "" }

// A backup asked for while one runs is refused, not run alongside it: both
// would read one manifest and delete what the other just sent.
func TestOnceOneAtATime(t *testing.T) {
	_, s3 := newFakeS3(t)
	st := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	pool := t.TempDir()
	b := New(&Config{S3: s3, Key: make([]byte, 32)}, "node", pool, st, nil, slog.New(slog.DiscardHandler))
	if _, ok, err := ReadStatus(pool); ok || err != nil {
		t.Errorf("ReadStatus() before any backup = %t, %v; want none", ok, err)
	}
	first := make(chan error, 1)
	go func() { first <- b.Once(t.Context()) }()
	<-st.entered
	if err := b.Once(t.Context()); !errors.Is(err, ErrBusy) {
		t.Errorf("a second backup during the first = %v, want ErrBusy", err)
	}
	close(st.release)
	if err := <-first; err == nil || errors.Is(err, ErrBusy) {
		t.Errorf("the first backup = %v, want the store's error", err)
	}
	// The one that ran left its result; the one refused changed nothing.
	got, ok, err := ReadStatus(pool)
	if !ok || err != nil || got.Error != "stopped for the test" || !got.Good.IsZero() || got.At.IsZero() {
		t.Errorf("ReadStatus() after a failed backup = %+v, %t, %v; want its error, its time and no good one yet", got, ok, err)
	}
}

// A backup that works after one that failed clears the failure, and a
// failure after it keeps when the last good one was.
func TestStatusKeepsTheLastGoodBackup(t *testing.T) {
	b := &Backer{pool: t.TempDir()}
	for _, step := range []struct {
		err      error
		wantGood bool // the last good backup is this one
	}{
		{errors.New("no route to host"), false},
		{nil, true},
		{errors.New("no route to host"), false},
	} {
		if err := b.record(step.err); err != nil {
			t.Fatalf("record(%v) error: %v", step.err, err)
		}
		got, ok, err := ReadStatus(b.pool)
		if !ok || err != nil {
			t.Fatalf("ReadStatus() = %t, %v", ok, err)
		}
		if (got.Error == "") != (step.err == nil) || got.Good.Equal(got.At) != step.wantGood {
			t.Errorf("after record(%v): %+v; want the error kept, and the last good backup this one: %t", step.err, got, step.wantGood)
		}
	}
	if got, _, _ := ReadStatus(b.pool); got.Good.IsZero() {
		t.Error("a failure after a good backup forgot when the good one was")
	}
}
