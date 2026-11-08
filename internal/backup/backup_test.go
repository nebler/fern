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
func (s *blockingStore) BackupIndex(context.Context, string) error                   { return nil }

// A backup asked for while one runs is refused, not run alongside it: both
// would read one manifest and delete what the other just sent.
func TestOnceOneAtATime(t *testing.T) {
	_, s3 := newFakeS3(t)
	st := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	b := New(&Config{S3: s3, Key: make([]byte, 32)}, "node", t.TempDir(), st, nil, slog.New(slog.DiscardHandler))
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
}
