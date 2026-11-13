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

// newT3 starts a T3 that answers its well-known path with status, until
// set says otherwise. Call it outside a synctest bubble, as newReceiver.
func newT3(t *testing.T) (*httptest.Server, func(status int)) {
	t.Helper()
	var mu sync.Mutex
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/t3/environment" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func(s int) {
		mu.Lock()
		defer mu.Unlock()
		status = s
	}
}

// A Heartbeat GETs its URL at its start and every interval while the store
// and T3 answer, sends nothing while either doesn't, and logs why once an
// outage, without the URL.
func TestHeartbeat(t *testing.T) {
	const beat = 5 * time.Minute
	steps := []struct {
		name   string
		answer int  // the watcher's answer from now on; 0: as before
		broken bool // the store doesn't answer
		t3     int  // T3's answer from now on; 0: as before
		looks  int  // intervals to wait
		want   int  // beats that arrive
	}{
		{name: "a beat each interval", looks: 3, want: 3},
		{name: "the watcher fails", answer: http.StatusBadGateway, looks: 2, want: 2},
		{name: "the watcher answers again", answer: http.StatusOK, looks: 1, want: 1},
		{name: "the store doesn't answer: silence", broken: true, looks: 3, want: 0},
		{name: "it answers again", looks: 2, want: 2},
		{name: "T3 doesn't answer: silence", t3: http.StatusServiceUnavailable, looks: 2, want: 0},
		{name: "T3 answers again", t3: http.StatusOK, looks: 1, want: 1},
	}
	w := newBeats(t)
	t3, setT3 := newT3(t)
	synctest.Test(t, func(t *testing.T) {
		st := &brokenStore{}
		var log logBuffer
		h := health.Heartbeat{Store: st, T3: &health.T3{URL: t3.URL}, URL: w.srv.URL + "/ping/secret", Log: slog.New(slog.NewTextHandler(&log, nil))}
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
			if step.t3 != 0 {
				setT3(step.t3)
			}
			time.Sleep(time.Duration(step.looks) * beat)
			synctest.Wait()
			if got := w.take(); got != step.want {
				t.Errorf("%s: %d beats, want %d", step.name, got, step.want)
			}
		}
		// Three outages: each is in the log once, and so is its end.
		for part, want := range map[string]int{
			"the heartbeat failed": 3, "answered 502": 1, "the store doesn't answer": 1, "T3 doesn't answer": 1,
			"answered 503": 1, "the heartbeat goes through again": 3,
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

// Without a T3 (--t3=""), only the store counts; one that is down stops
// the beats, and a store that stops answering next is logged too.
func TestHeartbeatT3(t *testing.T) {
	w := newBeats(t)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // nothing listens
	tests := []struct {
		name string
		t3   *health.T3
		want int
	}{
		{name: "no T3", want: 1},
		{name: "T3 down", t3: &health.T3{URL: down.URL}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := &brokenStore{}
				var log logBuffer
				h := health.Heartbeat{Store: st, T3: tt.t3, URL: w.srv.URL, Log: slog.New(slog.NewTextHandler(&log, nil))}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					h.Run(ctx, time.Minute)
					close(done)
				}()
				defer func() { cancel(); <-done }()
				time.Sleep(30 * time.Second)
				synctest.Wait()
				if got := w.take(); got != tt.want {
					t.Errorf("beats = %d, want %d", got, tt.want)
				}
				st.set(true)
				time.Sleep(time.Minute)
				synctest.Wait()
				// No T3: the store's failure, once. T3 down: its failure, and
				// the store's.
				if got := strings.Count(log.String(), "the heartbeat failed"); got != 2-tt.want {
					t.Errorf("failures logged %d times:\n%s", got, log.String())
				}
			})
		})
	}
}
