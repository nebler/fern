package health_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nebler/fern/internal/health"
	"github.com/nebler/fern/internal/store"
)

// beats counts the GETs a watcher receives, and answers them with status.
type beats struct {
	mu     sync.Mutex
	n      int
	other  []string // requests that weren't a plain GET
	status int
	srv    *httptest.Server
}

// newBeats starts a watcher. Call it outside a synctest bubble, as
// newReceiver.
func newBeats(t *testing.T) *beats {
	t.Helper()
	b := &beats{status: http.StatusOK}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if req.Method != http.MethodGet {
			b.other = append(b.other, req.Method)
		}
		b.n++
		w.WriteHeader(b.status)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// take returns how many beats arrived since the last take.
func (b *beats) take() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.n
	b.n = 0
	return n
}

func (b *beats) answer(status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = status
}

// brokenStore is a store that may stop answering.
type brokenStore struct {
	fakeStore
	mu     sync.Mutex
	broken bool
}

func (s *brokenStore) List(ctx context.Context) ([]store.Env, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return nil, errors.New("read /var/lib/fern/envs: input/output error")
	}
	return s.fakeStore.List(ctx)
}

func (s *brokenStore) set(broken bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken = broken
}

// A Heartbeat GETs its URL at its start and every interval while the store
// answers, sends nothing while it doesn't, and logs a failure once an
// outage, without the URL.
func TestHeartbeat(t *testing.T) {
	const beat = 5 * time.Minute
	steps := []struct {
		name   string
		answer int  // the watcher's answer from now on; 0: as before
		broken bool // the store doesn't answer
		looks  int  // intervals to wait
		want   int  // beats that arrive
	}{
		{name: "a beat each interval", looks: 3, want: 3},
		{name: "the watcher fails", answer: http.StatusBadGateway, looks: 2, want: 2},
		{name: "the watcher answers again", answer: http.StatusOK, looks: 1, want: 1},
		{name: "the store doesn't answer: silence", broken: true, looks: 3, want: 0},
		{name: "it answers again", looks: 2, want: 2},
	}
	w := newBeats(t)
	synctest.Test(t, func(t *testing.T) {
		st := &brokenStore{}
		var log logBuffer
		h := health.Heartbeat{Store: st, URL: w.srv.URL + "/ping/secret", Log: slog.New(slog.NewTextHandler(&log, nil))}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			h.Run(ctx, beat)
			close(done)
		}()
		defer func() { cancel(); <-done }()
		// The beat at the start, then the test looks half an interval after
		// each beat.
		time.Sleep(beat / 2)
		synctest.Wait()
		if got := w.take(); got != 1 {
			t.Errorf("beats at the start = %d, want 1", got)
		}
		for _, step := range steps {
			if step.answer != 0 {
				w.answer(step.answer)
			}
			st.set(step.broken)
			time.Sleep(time.Duration(step.looks) * beat)
			synctest.Wait()
			if got := w.take(); got != step.want {
				t.Errorf("%s: %d beats, want %d", step.name, got, step.want)
			}
		}
		// Two outages: each is in the log once, and so is its end.
		for part, want := range map[string]int{
			"the heartbeat failed": 2, "answered 502": 1, "the store doesn't answer": 1, "the heartbeat goes through again": 2,
		} {
			if got := strings.Count(log.String(), part); got != want {
				t.Errorf("%q logged %d times, want %d:\n%s", part, got, want, log.String())
			}
		}
		if strings.Contains(log.String(), "secret") {
			t.Errorf("the log names the URL:\n%s", log.String())
		}
	})
	if len(w.other) != 0 {
		t.Errorf("requests that weren't a GET: %q", w.other)
	}
}
