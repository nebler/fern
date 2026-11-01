// Package preview is the front door for previews: it serves the ports of
// environments' boxes over HTTP, routed by the request's host, waking a box
// that is asleep. http://5173-ENV.HOST/ is port 5173 of environment ENV's
// box, and http://ENV.HOST/ its default preview, the port of the first
// service in its services.yaml that has one. With HOST "localhost" a browser
// needs no DNS: every name under .localhost is the machine itself.
//
// A request counts as a command in the box while it lasts (Boxes), so a page with a dev server's live reload open keeps the box
// awake, and it touches the box's human clock. Like T3's own port, the
// front door is never public: the node's firewall keeps it to the machine,
// a tunnel or a tailnet.
package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// Boxes wakes boxes and counts the requests to them (actor.Group).
type Boxes interface {
	// Preview wakes environment id's box if needed and counts a request to
	// it until done; cancel ends the request. addr is the box's exec
	// address, whose host is the box's.
	Preview(ctx context.Context, id string, cancel context.CancelFunc) (addr string, done func(), err error)
}

// Store finds environments.
type Store interface {
	Get(ctx context.Context, id string) (store.Env, error)
}

// refusedWait is how long a connection the box refuses is tried again: a
// dev server that a wake starts, or that restarts after a change of its
// config, listens a moment later.
const refusedWait = 5 * time.Second

// Server serves previews.
type Server struct {
	host      string
	store     Store
	boxes     Boxes
	transport *http.Transport
	log       *slog.Logger

	mu      sync.Mutex
	loading map[string]int // requests in progress, by environment
}

// New returns a Server for the previews under host, such as "localhost".
func New(host string, st Store, boxes Boxes, log *slog.Logger) *Server {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &Server{
		host: strings.ToLower(host), store: st, boxes: boxes, log: log, loading: make(map[string]int),
		transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				deadline := time.Now().Add(refusedWait)
				for {
					conn, err := dialer.DialContext(ctx, network, addr)
					if !errors.Is(err, syscall.ECONNREFUSED) || time.Now().After(deadline) {
						return conn, err
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(100 * time.Millisecond):
					}
				}
			},
			IdleConnTimeout:     30 * time.Second,
			MaxIdleConnsPerHost: 16,
		},
	}
}

// ServeHTTP passes the request on to the box's port it names, waking the
// box first.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, port, ok := s.route(r.Host)
	if !ok {
		http.Error(w, fmt.Sprintf("not a preview: open http://PORT-ENV.%s/ or http://ENV.%s/", s.host, s.host), http.StatusNotFound)
		return
	}
	if port == 0 {
		var err error
		if port, err = s.defaultPort(r.Context(), id); err != nil {
			s.fail(w, r, id, err)
			return
		}
		if port == 0 {
			http.Error(w, fmt.Sprintf("%s has no default preview: open http://PORT-%s.%s/, or give a service in services.yaml a port", id, id, s.host), http.StatusNotFound)
			return
		}
	}
	s.count(id, 1)
	defer s.count(id, -1)
	// A restore, sleep or delete of the environment ends the request.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	addr, done, err := s.boxes.Preview(ctx, id, cancel)
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	defer done()
	boxHost, _, err := net.SplitHostPort(addr)
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(boxHost, strconv.Itoa(port))}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The dev server sees the name the browser used, for the URLs it
			// writes (its live reload's WebSocket among them).
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: s.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, syscall.ECONNREFUSED) {
				http.Error(w, fmt.Sprintf("nothing listens on port %d in %s's box: start its dev server (Claude: Bash with run_in_background), or declare it in services.yaml", port, id), http.StatusBadGateway)
				return
			}
			if r.Context().Err() == nil {
				s.log.WarnContext(r.Context(), "a preview request failed", slog.String("env", id), slog.Int("port", port), slog.Any("error", err))
			}
			http.Error(w, "the box didn't answer: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// route returns the environment and port a request's host names: port 0 is
// the default preview. A host that isn't one of the previews' is not ok.
func (s *Server) route(hostport string) (id string, port int, ok bool) {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	label, found := strings.CutSuffix(strings.ToLower(host), "."+s.host)
	if !found || label == "" || strings.Contains(label, ".") {
		return "", 0, false
	}
	// PORT-ENV: an environment whose ID starts with digits and a dash
	// needs its port named.
	if p, env, cut := strings.Cut(label, "-"); cut && env != "" {
		if n, err := strconv.Atoi(p); err == nil && p[0] != '+' {
			if n <= 0 || n > 65535 || n == boxexec.Port {
				return "", 0, false
			}
			port, label = n, env
		}
	}
	if store.CheckEnvID(label) != nil {
		return "", 0, false
	}
	return label, port, true
}

// defaultPort returns environment id's default preview port, 0 for none.
func (s *Server) defaultPort(ctx context.Context, id string) (int, error) {
	env, err := s.store.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	return setup.PreviewPort(env.Workspace)
}

// fail answers a request that couldn't reach environment id's box.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no environment "+id, http.StatusNotFound)
	case r.Context().Err() != nil:
		// The browser has gone.
	default:
		s.log.WarnContext(r.Context(), "a preview's box didn't wake", slog.String("env", id), slog.Any("error", err))
		w.Header().Set("Retry-After", "5")
		http.Error(w, "the box isn't up: "+err.Error(), http.StatusServiceUnavailable)
	}
}

// count adds n to environment id's requests in progress.
func (s *Server) count(id string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loading[id] += n; s.loading[id] <= 0 {
		delete(s.loading, id)
	}
}

func (s *Server) isLoading(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loading[id] > 0
}

// Warm renders the default preview of each environment woken names, once,
// as a browser's first request would, until ctx is done: a dev server
// compiles a page at its first request (TanStack Start: 10 s in a box), and
// the person who opens it later finds it done. An environment whose preview
// is loading already, as when a browser woke it, isn't rendered again.
func (s *Server) Warm(ctx context.Context, woken <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-woken:
			s.warm(ctx, id)
		}
	}
}

// warmWait bounds one render ahead: the dev server's start and its first
// page.
const warmWait = 2 * time.Minute

func (s *Server) warm(ctx context.Context, id string) {
	port, err := s.defaultPort(ctx, id)
	if err != nil || port == 0 {
		return
	}
	// A browser's request that woke the box is counted by now.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	if s.isLoading(id) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, warmWait)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+id+"."+s.host+"/", nil)
	if err != nil {
		return
	}
	for ctx.Err() == nil {
		rec := &discard{header: make(http.Header)}
		s.ServeHTTP(rec, req)
		// Nothing listens yet: the dev server is starting.
		if rec.code != http.StatusBadGateway {
			s.log.InfoContext(ctx, "preview rendered ahead", slog.String("env", id), slog.Int("status", rec.code),
				slog.Duration("took", time.Since(start)))
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// discard is a ResponseWriter that keeps only the status.
type discard struct {
	header http.Header
	code   int
}

func (d *discard) Header() http.Header { return d.header }

func (d *discard) Write(p []byte) (int, error) {
	if d.code == 0 {
		d.code = http.StatusOK
	}
	return len(p), nil
}

func (d *discard) WriteHeader(code int) {
	if d.code == 0 {
		d.code = code
	}
}
