package hibernate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/boxexec"
)

// An OpenCode server is not a process T3 talks to through its stdin, as
// Claude is, but one it calls over HTTP: T3 starts `opencode serve` and
// reads the server's URL from its output. Serve stands in for that server.
// It listens where T3 asked the server to, and passes T3's requests on to
// the server in the box. Once no request and no execution (a turn, a
// subagent) has been under way for a while, it stops the server, which keeps
// its sessions on disk, and the box sleeps. The next request starts it
// again, adds back the MCP servers T3 added at run time, which a restarted
// server forgets, and then goes in. T3's event stream stays open meanwhile:
// Serve keeps it alive with heartbeats, as the server does, and passes on
// each server's events in turn. To T3 it is the same server all along, a
// slower one at the first request after a stop.

const (
	// serverStartTimeout is how long a server may take to say where it
	// listens: a box's wake and OpenCode's start, about 3 s on the VPS.
	serverStartTimeout = time.Minute
	// heartbeat is how often T3's event stream gets a comment. T3 gives up on
	// a stream after 45 s of silence; OpenCode sends one every 15 s.
	heartbeat = 15 * time.Second
	// maxReplayed is the largest request body Serve keeps to send again.
	maxReplayed = 1 << 20
	// mcpPath is where T3 adds and removes its MCP servers.
	mcpPath = "/api/experimental/mcp/"
)

// listeningRE finds the server's URL in what `opencode serve` prints, as T3
// does.
var listeningRE = regexp.MustCompile(`server listening on\s+(https?://\S+)`)

// Serves says whether req starts an OpenCode server that Serve can stop
// and start again unseen: `opencode serve`, without a terminal.
func Serves(req boxexec.Request) bool {
	return !req.TTY && len(req.Argv) >= 2 && filepath.Base(req.Argv[0]) == "opencode" && req.Argv[1] == "serve"
}

// Serve runs the OpenCode server req starts with exec behind a proxy that
// listens at req's --hostname and --port, stops it after idle without
// anything under way, and starts it again at the next request. It prints
// the proxy's URL as the server would, and returns when s.Signal or ctx
// ends it, with the code of the server's last exit. Any other command, or
// an idle of 0, just runs.
func Serve(ctx context.Context, exec Exec, req boxexec.Request, s boxexec.Streams, idle time.Duration) (int, error) {
	if idle <= 0 || !Serves(req) {
		return exec(ctx, req, s)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listenAddr(req.Argv))
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(ctx)
	p := newProxy(exec, req, s.Stderr)
	var wg sync.WaitGroup
	defer wg.Wait() // after cancel, which runs first
	defer cancel()

	up, err := p.launch(ctx, &wg)
	if err != nil {
		_ = ln.Close()
		return p.exitCode(), err
	}
	p.started(up, nil)
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	wg.Go(func() { _ = srv.Serve(ln) })
	defer p.handlers.Wait()
	defer p.transport.CloseIdleConnections()
	defer func() { _ = srv.Close() }()
	if _, err := fmt.Fprintf(s.Stdout, "opencode server listening on http://%s\n", ln.Addr()); err != nil {
		return 0, err
	}

	tick := time.NewTicker(min(max(idle/10, 10*time.Millisecond), time.Second))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return p.exitCode(), ctx.Err()
		case sig := <-s.Signal:
			// T3 ending the server: stop the one running, if any.
			if up := p.stopping(); up != nil {
				up.sigs <- sig
				select {
				case <-up.exited:
				case <-time.After(stopTimeout):
				}
			}
			return p.exitCode(), nil
		case <-p.wakeup:
			if p.starting() {
				wg.Go(func() { p.started(p.launch(ctx, &wg)) })
			}
		case <-tick.C:
			p.stopIdle(idle)
		}
	}
}

// listenAddr returns where T3 asked the server to listen: the --hostname
// and --port it passes as `--name=value`, OpenCode's defaults otherwise.
func listenAddr(argv []string) string {
	host, port := "127.0.0.1", "4096"
	for _, arg := range argv[2:] {
		if v, ok := strings.CutPrefix(arg, "--hostname="); ok {
			host = v
		}
		if v, ok := strings.CutPrefix(arg, "--port="); ok {
			port = v
		}
	}
	return net.JoinHostPort(host, port)
}

// proxy is one Serve: the server, the servers standing in for it one after
// another, and T3's requests and event streams. Its fields after mu are
// shared by the handlers, the main loop and each server's goroutines.
type proxy struct {
	exec      Exec
	req       boxexec.Request
	stderr    io.Writer
	auth      string // the Authorization T3 sends; empty if it sends none
	transport *http.Transport
	wakeup    chan struct{} // a request waits for a server; capacity 1
	handlers  sync.WaitGroup

	mu       sync.Mutex
	changed  chan struct{} // closed, and replaced, when up or launching changes
	up       *upstream     // the server running; nil while stopped or starting
	launches bool          // a server is starting
	err      error         // why the last start failed
	code     int           // the code of the server's last exit
	busy     int           // requests being passed on
	quiet    time.Time     // since when busy and running have been zero
	running  map[string]struct{}
	hello    []byte                   // the first server's server.connected event
	subs     map[chan []byte]struct{} // T3's event streams
	mcp      map[string]resent        // T3's MCP servers, by path and query
}

// upstream is one server process in the box.
type upstream struct {
	base     *url.URL
	sigs     chan os.Signal
	cancel   context.CancelFunc // cuts its exec off
	exited   chan struct{}      // closed once code is set
	code     int
	stopping time.Time // when it was sent SIGTERM for being idle, or T3's signal; zero before
}

// resent is a request Serve sends again to a restarted server.
type resent struct {
	header http.Header
	body   []byte
}

func newProxy(exec Exec, req boxexec.Request, stderr io.Writer) *proxy {
	p := &proxy{
		exec: exec, req: req, stderr: stderr,
		transport: &http.Transport{MaxIdleConnsPerHost: 16, IdleConnTimeout: time.Minute},
		wakeup:    make(chan struct{}, 1),
		changed:   make(chan struct{}),
		running:   map[string]struct{}{},
		subs:      map[chan []byte]struct{}{},
		mcp:       map[string]resent{},
	}
	for _, v := range req.Environ {
		if pw, ok := strings.CutPrefix(v, "OPENCODE_PASSWORD="); ok && pw != "" {
			p.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+pw))
		}
	}
	return p
}

// notify tells the requests waiting on a server that something changed.
// p.mu is held.
func (p *proxy) notify() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *proxy) exitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.code
}

// starting says whether the main loop should start a server, and marks one
// as starting if so.
func (p *proxy) starting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.up != nil || p.launches {
		return false
	}
	p.launches = true
	p.notify()
	return true
}

// started records the end of a start.
func (p *proxy) started(up *upstream, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.launches, p.err = false, err
	if err == nil {
		p.up, p.quiet = up, time.Now()
	}
	p.notify()
}

// stopping marks the server running, if any, as stopping, and returns it.
func (p *proxy) stopping() *upstream {
	p.mu.Lock()
	defer p.mu.Unlock()
	up := p.up
	if up != nil && up.stopping.IsZero() {
		up.stopping = time.Now()
		p.notify()
	}
	return up
}

// stopIdle stops the server once nothing has been under way for idle, and
// cuts off one that takes too long to stop.
func (p *proxy) stopIdle(idle time.Duration) {
	p.mu.Lock()
	up := p.up
	switch {
	case up == nil:
		p.mu.Unlock()
	case !up.stopping.IsZero():
		late := time.Since(up.stopping) >= stopTimeout
		p.mu.Unlock()
		if late {
			up.cancel()
		}
	case p.busy == 0 && len(p.running) == 0 && time.Since(p.quiet) >= idle:
		up.stopping = time.Now()
		p.notify()
		p.mu.Unlock()
		up.sigs <- syscall.SIGTERM
	default:
		p.mu.Unlock()
	}
}

// launch starts a server, waits until it listens, adds back T3's MCP
// servers and subscribes to its events. The server's goroutines join wg.
func (p *proxy) launch(ctx context.Context, wg *sync.WaitGroup) (*upstream, error) {
	ctx, cancel := context.WithCancel(ctx)
	up := &upstream{sigs: make(chan os.Signal, 4), cancel: cancel, exited: make(chan struct{})}
	listening := make(chan string, 1)
	out := &lineWriter{w: io.Discard, keep: func(line []byte) bool {
		if m := listeningRE.FindSubmatch(line); m != nil {
			select {
			case listening <- string(m[1]):
			default:
			}
		}
		return false
	}}
	wg.Go(func() {
		defer cancel()
		code, err := p.exec(ctx, p.req, boxexec.Streams{Stdout: out, Stderr: p.stderr, Signal: up.sigs})
		if err != nil && code == 0 {
			code = 128 + int(syscall.SIGTERM)
		}
		up.code = code
		close(up.exited)
		p.gone(up)
	})
	fail := func(err error) (*upstream, error) {
		cancel()
		<-up.exited
		return nil, err
	}
	timer := time.NewTimer(serverStartTimeout)
	defer timer.Stop()
	select {
	case raw := <-listening:
		base, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil {
			return fail(fmt.Errorf("opencode's URL %q: %w", raw, err))
		}
		up.base = base
	case <-up.exited:
		return nil, fmt.Errorf("opencode exited with %d before it listened", up.code)
	case <-timer.C:
		return fail(errors.New("opencode did not listen within a minute"))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	if err := p.replay(ctx, up); err != nil {
		return fail(err)
	}
	events, err := p.subscribe(ctx, up)
	if err != nil {
		return fail(err)
	}
	// Its first event, server.connected, before T3 hears of it: each of T3's
	// streams begins with the first server's.
	br := bufio.NewReaderSize(events, 64<<10)
	frame, data, err := readEvent(br)
	if err != nil {
		_ = events.Close()
		return fail(fmt.Errorf("opencode's events: %w", err))
	}
	p.event(frame, data)
	wg.Go(func() {
		defer func() { _ = events.Close() }()
		p.follow(br)
		if !p.stopped(up) {
			// The server stopped sending events by itself: T3 must reconnect
			// to see what it missed, so this server goes.
			up.cancel()
		}
	})
	return up, nil
}

// stopped says whether up was stopped on purpose.
func (p *proxy) stopped(up *upstream) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !up.stopping.IsZero()
}

// gone records a server's exit. One stopped on purpose leaves T3's event
// streams open; one that ended by itself takes them along, with the
// executions it lost, so T3 reconnects and asks what became of them.
func (p *proxy) gone(up *upstream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code = up.code
	if p.up != up {
		return
	}
	p.up = nil
	if up.stopping.IsZero() {
		for ch := range p.subs {
			close(ch)
			delete(p.subs, ch)
		}
		clear(p.running)
		p.quiet = time.Now()
	}
	p.notify()
}

// replay adds T3's MCP servers to a new server.
func (p *proxy) replay(ctx context.Context, up *upstream) error {
	p.mu.Lock()
	calls := make(map[string]resent, len(p.mcp))
	for k, v := range p.mcp {
		calls[k] = v
	}
	p.mu.Unlock()
	for path, call := range calls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.base.String()+path, bytes.NewReader(call.body))
		if err != nil {
			return err
		}
		req.Header = call.header.Clone()
		resp, err := p.transport.RoundTrip(req)
		if err != nil {
			return fmt.Errorf("add MCP server %s again: %w", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// A refused one stays refused; T3's next turn finds it missing.
		if resp.StatusCode/100 != 2 {
			_, _ = fmt.Fprintf(p.stderr, "fern: opencode refused MCP server %s again: %s\n", path, resp.Status)
		}
	}
	return nil
}

// subscribe opens a server's event stream.
func (p *proxy) subscribe(ctx context.Context, up *upstream) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, up.base.String()+"/api/event", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if p.auth != "" {
		req.Header.Set("Authorization", p.auth)
	}
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("opencode's events: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("opencode's events: %s", resp.Status)
	}
	return resp.Body, nil
}

// follow reads a server's events until they end, keeps track of the
// executions under way and passes the events on to T3's streams. A
// server's server.connected is passed on only once, to each stream as it
// opens: the streams outlive the servers.
func (p *proxy) follow(br *bufio.Reader) {
	for {
		frame, data, err := readEvent(br)
		if err != nil {
			return
		}
		p.event(frame, data)
	}
}

// readEvent reads the next event of a stream: its frame, with the blank
// line that ends it, and its data. Comments (heartbeats) are skipped.
func readEvent(br *bufio.Reader) (frame, data []byte, err error) {
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, nil, err
		}
		if len(bytes.TrimRight(line, "\r\n")) > 0 {
			frame = append(frame, line...)
			if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				data = append(data, bytes.TrimSpace(v)...)
			}
			continue
		}
		if len(data) > 0 {
			return append(frame, '\n'), data, nil
		}
		frame = nil
	}
}

// event takes one event from the server.
func (p *proxy) event(frame, data []byte) {
	var ev struct {
		Type string `json:"type"`
		Data struct {
			SessionID string `json:"sessionID"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &ev)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch ev.Type {
	case "server.connected":
		if p.hello == nil {
			p.hello = frame
		}
		return
	case "session.execution.started":
		p.running[ev.Data.SessionID] = struct{}{}
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		delete(p.running, ev.Data.SessionID)
		if len(p.running) == 0 && p.busy == 0 {
			p.quiet = time.Now()
		}
	}
	for ch := range p.subs {
		select {
		case ch <- frame:
		default:
			// Too far behind: T3 reconnects and asks what it missed.
			close(ch)
			delete(p.subs, ch)
		}
	}
}

// ServeHTTP passes T3's requests on to the server, starting it if needed,
// and serves T3's event streams itself.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.handlers.Add(1)
	defer p.handlers.Done()
	if p.auth != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(p.auth)) != 1 {
		// Refused here, so a stray request doesn't wake the box.
		w.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/event" {
		p.events(w, r)
		return
	}
	var body []byte
	if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, mcpPath) {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxReplayed+1))
		if err != nil || len(b) > maxReplayed {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	}
	up, err := p.acquire(r.Context())
	if err != nil {
		http.Error(w, "fern: opencode: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer p.release()
	rp := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { pr.SetURL(up.base) },
		Transport:     p.transport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode/100 == 2 {
				p.record(r, body)
			}
			return nil
		},
	}
	rp.ServeHTTP(w, r)
}

// record keeps T3's MCP servers, to add them again to the next server.
func (p *proxy) record(r *http.Request, body []byte) {
	if !strings.HasPrefix(r.URL.Path, mcpPath) || strings.Contains(r.URL.Path[len(mcpPath):], "/") {
		return
	}
	key := r.URL.Path + "?" + r.URL.RawQuery
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		h := http.Header{}
		for _, k := range []string{"Authorization", "Content-Type"} {
			if v := r.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		p.mcp[key] = resent{header: h, body: body}
	case http.MethodDelete:
		delete(p.mcp, key)
	}
}

// acquire returns the running server, starting one if needed, and counts
// the request as under way.
func (p *proxy) acquire(ctx context.Context) (*upstream, error) {
	p.mu.Lock()
	asked := false
	for {
		if p.up != nil && p.up.stopping.IsZero() {
			p.busy++
			up := p.up
			p.mu.Unlock()
			return up, nil
		}
		if asked && p.up == nil && !p.launches && p.err != nil {
			err := p.err
			p.mu.Unlock()
			return nil, err
		}
		if p.up == nil && !p.launches {
			select {
			case p.wakeup <- struct{}{}:
			default:
			}
			asked = true
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.mu.Lock()
	}
}

// release ends a request acquire counted.
func (p *proxy) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy--
	if p.busy == 0 && len(p.running) == 0 {
		p.quiet = time.Now()
	}
}

// events serves one of T3's event streams: the first server's
// server.connected, then every server's events, with a heartbeat between
// them. It ends when T3 closes it or its server ends by itself.
func (p *proxy) events(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	ch := make(chan []byte, 1024)
	p.mu.Lock()
	hello := p.hello
	p.subs[ch] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.subs, ch)
		p.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	write := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if hello != nil && !write(hello) {
		return
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case frame, ok := <-ch:
			if !ok || !write(frame) {
				return
			}
		case <-tick.C:
			if !write([]byte(": heartbeat\n\n")) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}
