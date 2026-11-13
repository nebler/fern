package health_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/health"
	"github.com/nebler/fern/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// node is the node's problems as a test sets them: a store and the boxes
// that keep crashing, both safe to change while a Notifier looks.
type node struct {
	mu    sync.Mutex
	store fakeStore
	boxes fakeBoxes
}

func (n *node) set(change func(n *node)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	change(n)
}

func (n *node) List(ctx context.Context) ([]store.Env, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.store.List(ctx)
}

func (n *node) BuildFailure(ctx context.Context, id string) (store.BuildFailure, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.store.BuildFailure(ctx, id)
}

func (n *node) SnapshotFailure(ctx context.Context, id string) (store.SnapshotFailure, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.store.SnapshotFailure(ctx, id)
}

func (n *node) Space() (store.Space, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.store.Space()
}

func (n *node) Crashing(ctx context.Context) ([]actor.Crashing, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.boxes.Crashing(ctx)
}

// receiver is where a Notifier posts: it keeps each message, and answers
// with status.
type receiver struct {
	mu     sync.Mutex
	got    []string
	status int
	srv    *httptest.Server
}

// newReceiver starts a receiver. Call it outside a synctest bubble: a
// server's goroutines wait on the network, which a bubble's clock would
// wait for.
func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			r.got = append(r.got, "not a POST of plain text: "+req.Method+" "+req.Header.Get("Content-Type"))
		}
		r.got = append(r.got, string(body))
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// take returns the messages received since the last take.
func (r *receiver) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.got
	r.got = nil
	return got
}

func (r *receiver) answer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// logBuffer is a log safe to read while a Notifier writes it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// told returns the keys of the problems file says were told of, an empty
// list if it says none; nil if there is no file.
func told(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Sent map[string]time.Time `json:"sent"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("%s: %v: %s", file, err, data)
	}
	return append([]string{}, slices.Sorted(maps.Keys(f.Sent))...)
}

const every = time.Minute

// run starts n's Run in the bubble, and returns what stops it and waits for
// it.
func run(t *testing.T, n health.Notifier) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		n.Run(ctx, every)
		close(done)
	}()
	return func() { cancel(); <-done }
}

// A Notifier tells of a problem once when it appears, whatever its details
// do after, and once when the last has cleared; a restart of the daemon
// doesn't tell again, and a receiver that is down is tried at every look
// and logged once.
func TestNotifier(t *testing.T) {
	const doctor = "\n`fern doctor` on the server says what to do."
	full := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 1 << 30}, Least: 5 << 30}
	roomy := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 50 << 30}, Least: 5 << 30}
	disk := "disk: no new snapshots or threads: " + full.Full().Error()
	t0 := time.Date(2026, 11, 13, 14, 2, 0, 0, time.UTC)
	crashing := func(times int) fakeBoxes {
		return fakeBoxes{{Box: "t3-1", Times: times, Since: t0, Last: t0.Add(time.Minute), Retry: t0.Add(5 * time.Minute), Cause: "out of memory"}}
	}
	box := "box: t3-1 keeps crashing: it died or failed to wake 3 times since 14:02 UTC, and its commands fail until 14:07 UTC, when the next one tries again. The last time: out of memory"
	build := "golden build: golden-app-12345678: its threads start without what the setup script and the services' first start make: pnpm: not found"

	steps := []struct {
		name    string
		change  func(n *node)
		answer  int      // what the receiver answers from now on; 0: as before
		restart bool     // the daemon restarts first
		looks   int      // how many looks to wait for; 0: one
		want    []string // the messages received meanwhile
		told    []string // the problems the file says were told of, after
	}{
		{name: "nothing wrong"},
		{
			name:   "the pool fills up",
			change: func(n *node) { n.store.space = full },
			want:   []string{"Fern on vps1: " + disk + doctor}, told: []string{"disk"},
		},
		{name: "it lasts", looks: 3, told: []string{"disk"}},
		{name: "the daemon restarts", restart: true, looks: 2, told: []string{"disk"}},
		{
			name: "two more appear at once",
			change: func(n *node) {
				n.boxes = crashing(3)
				n.store.envs = []string{"golden-app-12345678"}
				n.store.failed = map[string]store.BuildFailure{"golden-app-12345678": {Key: "k1", At: t0, Error: "pnpm: not found\nmore"}}
			},
			want: []string{"Fern on vps1: 2 problems\n" + box + "\n" + build + doctor},
			told: []string{"box t3-1", "disk", "golden build golden-app-12345678"},
		},
		{
			name: "their details change",
			change: func(n *node) {
				n.boxes = crashing(4)
				n.store.failed = map[string]store.BuildFailure{"golden-app-12345678": {Key: "k2", At: t0.Add(time.Hour), Error: "npm: not found"}}
			},
			looks: 2, told: []string{"box t3-1", "disk", "golden build golden-app-12345678"},
		},
		{
			name:   "two clear and one lasts",
			change: func(n *node) { n.boxes, n.store.space = nil, roomy },
			told:   []string{"golden build golden-app-12345678"},
		},
		{
			name:   "one comes back",
			change: func(n *node) { n.boxes = crashing(3) },
			want:   []string{"Fern on vps1: " + box + doctor}, told: []string{"box t3-1", "golden build golden-app-12345678"},
		},
		{
			name:   "the receiver is down as a problem appears",
			change: func(n *node) { n.store.space = full }, answer: http.StatusBadGateway, looks: 3,
			want: slices.Repeat([]string{"Fern on vps1: " + disk + doctor}, 3), told: []string{"box t3-1", "golden build golden-app-12345678"},
		},
		{
			name: "the receiver is back", answer: http.StatusOK, looks: 2,
			want: []string{"Fern on vps1: " + disk + doctor}, told: []string{"box t3-1", "disk", "golden build golden-app-12345678"},
		},
		{
			name:   "the last ones clear while the receiver is down",
			change: func(n *node) { n.boxes, n.store.space, n.store.failed = nil, roomy, nil }, answer: http.StatusBadGateway, looks: 2,
			want: slices.Repeat([]string{"Fern on vps1: nothing is wrong any more that the daemon knows of."}, 2),
			told: []string{"box t3-1", "disk", "golden build golden-app-12345678"},
		},
		{
			name: "and it is said once the receiver is back", answer: http.StatusOK, looks: 2,
			want: []string{"Fern on vps1: nothing is wrong any more that the daemon knows of."}, told: []string{},
		},
		{name: "the daemon restarts with nothing wrong", restart: true, looks: 2, told: []string{}},
	}

	rcv := newReceiver(t)
	synctest.Test(t, func(t *testing.T) {
		nd := &node{store: fakeStore{space: roomy}}
		var log logBuffer
		n := health.Notifier{
			Checker: health.Checker{Store: nd, Pool: t.TempDir(), Boxes: nd},
			URL:     rcv.srv.URL + "/topic", Node: "vps1", File: filepath.Join(t.TempDir(), health.NotifiedFile),
			Log: slog.New(slog.NewTextHandler(&log, nil)),
		}
		stop := run(t, n)
		defer func() { stop() }()
		// The test looks half a minute after each of the Notifier's looks.
		time.Sleep(every / 2)
		for _, step := range steps {
			if step.restart {
				stop()
				stop = run(t, n)
				time.Sleep(every / 2)
			}
			if step.change != nil {
				nd.set(step.change)
			}
			if step.answer != 0 {
				rcv.answer(step.answer)
			}
			time.Sleep(time.Duration(max(step.looks, 1)) * every)
			synctest.Wait()
			if diff := cmp.Diff(step.want, rcv.take()); diff != "" {
				t.Errorf("%s: messages (-want +got):\n%s", step.name, diff)
			}
			if diff := cmp.Diff(step.told, told(t, n.File)); diff != "" {
				t.Errorf("%s: the problems told of, on disk (-want +got):\n%s", step.name, diff)
			}
		}
		// Five POSTs failed, in two outages: each outage is in the log once,
		// and without the URL, whose topic is the receiver's only secret.
		if got := strings.Count(log.String(), "telling of the node's problems failed"); got != 2 {
			t.Errorf("failed POSTs logged %d times, want 2, once an outage:\n%s", got, log.String())
		}
		if strings.Contains(log.String(), "/topic") {
			t.Errorf("the log names the URL:\n%s", log.String())
		}
	})
}

// What is on disk when the daemon starts decides what it tells of again:
// nothing that the file says was told of, also beside a copy that a crash
// cut short; everything, when the file can't be read.
func TestNotifierAtStart(t *testing.T) {
	full := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 1 << 30}, Least: 5 << 30}
	for _, tc := range []struct {
		name  string
		files map[string]string // beside the file, by suffix
		want  int               // messages at the start
		told  []string          // on disk after
	}{
		{name: "no file", want: 1, told: []string{"disk"}},
		{name: "told of already", files: map[string]string{"": `{"sent":{"disk":"2026-11-13T14:00:00Z"}}`}, told: []string{"disk"}},
		{
			name:  "told of already, and a write a crash cut short",
			files: map[string]string{"": `{"sent":{"disk":"2026-11-13T14:00:00Z"}}`, ".new": `{"sent":{"di`}, told: []string{"disk"},
		},
		{name: "a file that can't be read", files: map[string]string{"": `{"sent":{"di`}, want: 1, told: []string{"disk"}},
		{
			name:  "told of something that is over, and not of this",
			files: map[string]string{"": `{"sent":{"backups":"2026-11-13T14:00:00Z"}}`}, want: 1, told: []string{"disk"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rcv := newReceiver(t)
			synctest.Test(t, func(t *testing.T) {
				n := health.Notifier{
					Checker: health.Checker{Store: fakeStore{space: full}, Pool: t.TempDir()},
					URL:     rcv.srv.URL, Node: "vps1", File: filepath.Join(t.TempDir(), health.NotifiedFile),
					Log: slog.New(slog.DiscardHandler),
				}
				for suffix, data := range tc.files {
					if err := os.WriteFile(n.File+suffix, []byte(data), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				stop := run(t, n)
				defer stop()
				time.Sleep(every / 2)
				synctest.Wait()
				if got := rcv.take(); len(got) != tc.want {
					t.Errorf("messages at the start = %q, want %d", got, tc.want)
				}
				if diff := cmp.Diff(tc.told, told(t, n.File)); diff != "" {
					t.Errorf("the problems told of, on disk (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// A receiver that never answers holds nothing up: the daemon's stop ends
// the POST and Run returns.
func TestNotifierStopsDuringAPost(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(arrived)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	full := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 1 << 30}, Least: 5 << 30}
	n := health.Notifier{
		Checker: health.Checker{Store: fakeStore{space: full}, Pool: t.TempDir()},
		URL:     srv.URL, Node: "vps1", File: filepath.Join(t.TempDir(), health.NotifiedFile), Log: slog.New(slog.DiscardHandler),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		n.Run(ctx, time.Hour)
		close(done)
	}()
	<-arrived
	cancel()
	<-done
	if got := told(t, n.File); got != nil {
		t.Errorf("a POST that never got an answer is recorded as told: %q", got)
	}
}

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://ntfy.sh/fern-3f9a", true},
		{"http://10.0.0.5:8080/hooks/fern?x=1", true},
		{"ntfy.sh/fern-3f9a", false},
		{"mailto:me@example.com", false},
		{"https://", false},
		{"", false},
	} {
		if err := health.CheckURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("CheckURL(%q) error = %v, want ok %t", tc.url, err, tc.ok)
		}
	}
}
