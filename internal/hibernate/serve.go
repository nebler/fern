package hibernate

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
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
//
// Serve is an actor: its goroutine (proxy.run) owns the server's phase and
// everything counted against it. The requests' handlers (serve_http.go) and
// each server's goroutines reach it only through its mailbox (proxy.tell).

const (
	// serverStartTimeout is how long a server may take to say where it
	// listens, and again to send its first event: a box's wake and
	// OpenCode's start, about 3 s on the VPS.
	serverStartTimeout = time.Minute
	// replayTimeout is how long adding one MCP server again may take: OpenCode
	// connects to it at once, and T3 allows as long for adding it.
	replayTimeout = 5 * time.Second
)

// listeningRE finds the server's URL in what `opencode serve` prints, as T3
// does.
var listeningRE = regexp.MustCompile(`server listening on\s+(https?://\S+)`)

// serverPhase is what a proxy's server is doing.
type serverPhase int

const (
	// serverStopped: none runs, and its box may sleep. The next request
	// starts one.
	serverStopped serverPhase = iota
	// serverStarting: one is on its way up. Requests wait for it.
	serverStarting
	// serverUp: one takes requests.
	serverUp
	// serverStopping: one was told to stop, for being idle or by T3.
	// Requests wait for its exit, and for the server after it.
	serverStopping
)

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
	p := newProxy(exec, req, s.Stderr, idle)
	// The servers' goroutines and the handlers end with ctx, or at their
	// next word to the actor once nobody reads its mailbox any more.
	unread := sync.OnceFunc(func() { close(p.done) })
	defer p.wg.Wait()
	defer unread()
	defer cancel()

	// The first server, before T3 is told where to call: if it can't start,
	// T3 hears why from this command's end, as from OpenCode's own.
	up, err := p.launch(ctx, nil)
	if err != nil {
		_ = ln.Close()
		return 0, err
	}
	p.launched(up, nil)

	// The connections are counted from the goroutine that accepts them, so
	// that once it has ended none is added while they are waited for.
	var conns sync.WaitGroup
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, ConnState: func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			conns.Add(1)
		case http.StateClosed, http.StateHijacked:
			conns.Done()
		case http.StateActive, http.StateIdle:
		}
	}}
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		_ = srv.Serve(ln)
	}()
	defer func() {
		unread()
		_ = srv.Close()
		<-accepting
		conns.Wait()
		p.transport.CloseIdleConnections()
	}()
	if _, err := fmt.Fprintf(s.Stdout, "opencode server listening on http://%s\n", ln.Addr()); err != nil {
		return 0, err
	}
	return p.run(ctx, s.Signal)
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

// proxy is one Serve: the servers standing in for T3's one after another,
// and T3's requests and event streams.
type proxy struct {
	// Set once; any goroutine may read them.
	exec      Exec
	req       boxexec.Request
	stderr    io.Writer
	auth      string // the Authorization T3 sends; empty if it sends none
	idle      time.Duration
	transport *http.Transport
	inbox     chan func(context.Context) // the actor's mailbox
	done      chan struct{}              // closed when Serve returns: nobody reads the mailbox any more
	wg        sync.WaitGroup             // the servers' goroutines

	// The actor's own: only run's goroutine touches what follows.
	phase   serverPhase
	server  *upstream                // the one up or stopping; nil otherwise
	since   time.Time                // up: since when nothing has been under way; stopping: when it was told to stop
	ending  bool                     // T3 is ending the server: its exit is Serve's
	code    int                      // the code of the server's last exit
	waiting []chan grant             // requests waiting for a server
	busy    int                      // requests being passed on
	running map[string]struct{}      // sessions with an execution under way
	hello   []byte                   // the first server's server.connected event
	subs    map[chan []byte]struct{} // T3's event streams
	mcp     map[string]resent        // T3's MCP servers, by path and query
	timer   *time.Timer              // fires when the server may be idle, or is late stopping
}

// upstream is one server process in the box.
type upstream struct {
	base   *url.URL
	pass   *httputil.ReverseProxy // passes requests on to base
	sigs   chan os.Signal
	cancel context.CancelFunc // cuts its exec off
	exited chan struct{}      // closed once code and err are set
	code   int
	err    error         // why its exec failed, if it did: no box, no daemon
	events io.ReadCloser // its event stream
	br     *bufio.Reader // reads events
	first  sse           // its first event
}

// grant answers a request that asked for a server.
type grant struct {
	up  *upstream
	err error
}

// resent is a request Serve sends again to a restarted server.
type resent struct {
	header http.Header
	body   []byte
}

func newProxy(exec Exec, req boxexec.Request, stderr io.Writer, idle time.Duration) *proxy {
	p := &proxy{
		exec: exec, req: req, stderr: stderr, idle: idle,
		transport: &http.Transport{MaxIdleConnsPerHost: 16, IdleConnTimeout: time.Minute},
		inbox:     make(chan func(context.Context)),
		done:      make(chan struct{}),
		running:   map[string]struct{}{},
		subs:      map[chan []byte]struct{}{},
		mcp:       map[string]resent{},
		timer:     time.NewTimer(0),
	}
	p.timer.Stop()
	for _, v := range req.Environ {
		if pw, ok := strings.CutPrefix(v, "OPENCODE_PASSWORD="); ok && pw != "" {
			p.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+pw))
		}
	}
	return p
}

// tell has the actor call fn, and says whether it will: not once Serve
// has returned.
func (p *proxy) tell(fn func(ctx context.Context)) bool {
	select {
	case p.inbox <- fn:
		return true
	case <-p.done:
		return false
	}
}

// run is the actor: it takes what the handlers and the servers' goroutines
// tell it, one at a time, until T3's signal or ctx ends it.
func (p *proxy) run(ctx context.Context, signals <-chan os.Signal) (int, error) {
	defer p.timer.Stop()
	for {
		if p.ending && p.server == nil {
			return p.code, nil
		}
		p.arm()
		select {
		case <-ctx.Done():
			return p.code, ctx.Err()
		case sig := <-signals:
			p.end(sig)
		case fn := <-p.inbox:
			fn(ctx)
		case <-p.timer.C:
			p.timeUp()
		}
	}
}

// idling says whether the server is up with nothing under way.
func (p *proxy) idling() bool {
	return p.phase == serverUp && p.busy == 0 && len(p.running) == 0
}

// arm sets the timer for what the phase waits for: an idle server's stop,
// or a stopping server's exit.
func (p *proxy) arm() {
	switch {
	case p.idling():
		p.timer.Reset(time.Until(p.since.Add(p.idle)))
	case p.phase == serverStopping:
		p.timer.Reset(time.Until(p.since.Add(stopTimeout)))
	default:
		p.timer.Stop()
	}
}

// timeUp stops a server that has been idle long enough, and cuts off one
// that takes too long to stop.
func (p *proxy) timeUp() {
	switch {
	case p.idling() && time.Since(p.since) >= p.idle:
		p.stop(syscall.SIGTERM)
	case p.phase == serverStopping && time.Since(p.since) >= stopTimeout:
		p.server.cancel()
		p.since = time.Now() // and again, should even that not end it
	}
}

// stop tells the server that is up to stop.
func (p *proxy) stop(sig os.Signal) {
	p.signal(sig)
	p.phase, p.since = serverStopping, time.Now()
}

// signal passes sig on to the server. Its exec reads them; one that has
// ended leaves them unread, and has no use for more.
func (p *proxy) signal(sig os.Signal) {
	select {
	case p.server.sigs <- sig:
	default:
	}
}

// end is T3 ending the server with sig: the one running stops, and run
// returns once none does.
func (p *proxy) end(sig os.Signal) {
	p.ending = true
	switch p.phase {
	case serverUp:
		p.stop(sig)
	case serverStopping:
		p.signal(sig)
	case serverStarting, serverStopped:
		// None runs yet, or any more: run returns at once, and a start
		// ends with Serve.
	}
}

// start starts a server in the background, for the requests waiting.
func (p *proxy) start(ctx context.Context) {
	p.phase = serverStarting
	calls := maps.Clone(p.mcp)
	p.wg.Go(func() {
		up, err := p.launch(ctx, calls)
		p.tell(func(context.Context) { p.launched(up, err) })
	})
}

// launched takes the end of a start: the server is up and the requests
// that waited for it go in, or they hear why it isn't.
func (p *proxy) launched(up *upstream, err error) {
	if err == nil {
		// One that has ended since is no server. Its exit, told
		// separately, finds another in p.server, or none.
		select {
		case <-up.exited:
			_ = up.events.Close()
			err = up.early()
		default:
		}
	}
	waiting := p.waiting
	p.waiting = nil
	if err != nil {
		p.phase = serverStopped
		for _, w := range waiting {
			w <- grant{err: err}
		}
		return
	}
	p.phase, p.server, p.since = serverUp, up, time.Now()
	p.event(up, up.first)
	// Followed from here on, so that none of its events comes before it is
	// p.server.
	p.wg.Go(func() { p.follow(up) })
	for _, w := range waiting {
		p.busy++
		w <- grant{up: up}
	}
}

// exited takes a server's exit. One stopped on purpose leaves T3's event
// streams open, and the next starts if requests wait for it. One that ended
// by itself takes the streams along, with the executions it lost, so T3
// reconnects and asks what became of them.
func (p *proxy) exited(ctx context.Context, up *upstream) {
	p.code = up.code
	if up != p.server {
		return // one that never came up; launched says so
	}
	byItself := p.phase == serverUp
	p.phase, p.server = serverStopped, nil
	if byItself {
		for ch := range p.subs {
			close(ch)
		}
		clear(p.subs)
		clear(p.running)
	}
	if len(p.waiting) > 0 && !p.ending {
		p.start(ctx)
	}
}

// acquire answers a request that wants the server on reply: at once if one
// is up, and otherwise after the start it causes or finds under way. A
// request granted one is counted as under way until release.
func (p *proxy) acquire(ctx context.Context, reply chan grant) {
	if p.phase == serverUp {
		p.busy++
		reply <- grant{up: p.server}
		return
	}
	p.waiting = append(p.waiting, reply)
	if p.phase == serverStopped {
		p.start(ctx)
	}
}

// abandon answers a request that gave up waiting with err, unless it was
// answered already.
func (p *proxy) abandon(reply chan grant, err error) {
	for i, w := range p.waiting {
		if w == reply {
			p.waiting = append(p.waiting[:i], p.waiting[i+1:]...)
			reply <- grant{err: err}
			return
		}
	}
}

// release ends a request acquire counted.
func (p *proxy) release() {
	p.busy--
	if p.busy == 0 && len(p.running) == 0 {
		p.since = time.Now()
	}
}

// event takes one event of up: it keeps track of the executions under way
// and passes the event on to T3's streams. A server's server.connected is
// passed on only once, to each stream as it opens: the streams outlive the
// servers.
func (p *proxy) event(up *upstream, ev sse) {
	if up != p.server {
		return // a server that has gone
	}
	switch ev.kind {
	case "server.connected":
		if p.hello == nil {
			p.hello = ev.frame
		}
		return
	case "session.execution.started":
		p.running[ev.session] = struct{}{}
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
		delete(p.running, ev.session)
		if len(p.running) == 0 && p.busy == 0 {
			p.since = time.Now()
		}
	}
	for ch := range p.subs {
		select {
		case ch <- ev.frame:
		default:
			// Too far behind: T3 reconnects and asks what it missed.
			close(ch)
			delete(p.subs, ch)
		}
	}
}

// eventsEnded takes the end of up's event stream. If the server wasn't
// told to stop, it stopped sending events by itself: T3 must reconnect to
// see what it missed, so this server goes.
func (p *proxy) eventsEnded(up *upstream) {
	if up == p.server && p.phase == serverUp {
		up.cancel()
	}
}

// subscribe adds one of T3's event streams and answers with the event it
// begins with, if there is one yet.
func (p *proxy) subscribe(ch chan []byte, hello chan<- []byte) {
	p.subs[ch] = struct{}{}
	hello <- p.hello
}

// unsubscribe drops one of T3's event streams, if it is still there.
func (p *proxy) unsubscribe(ch chan []byte) { delete(p.subs, ch) }

// setMCP keeps, or with a nil call forgets, one of T3's MCP servers.
func (p *proxy) setMCP(key string, call *resent) {
	if call == nil {
		delete(p.mcp, key)
		return
	}
	p.mcp[key] = *call
}

// launch starts a server, waits until it listens, adds T3's MCP servers
// (calls) to it and reads the first event of its stream. It runs outside
// the actor, which is told of the server's exit, whenever that is.
func (p *proxy) launch(ctx context.Context, calls map[string]resent) (*upstream, error) {
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
	p.wg.Go(func() {
		defer cancel()
		code, err := p.exec(ctx, p.req, boxexec.Streams{Stdout: out, Stderr: p.stderr, Signal: up.sigs})
		if err != nil && code == 0 {
			code = 128 + int(syscall.SIGTERM)
		}
		up.code, up.err = code, err
		close(up.exited)
		p.tell(func(ctx context.Context) { p.exited(ctx, up) })
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
		up.base, up.pass = base, p.passTo(base, nil)
	case <-up.exited:
		return nil, up.early()
	case <-timer.C:
		return fail(errors.New("opencode did not listen within a minute"))
	}
	for path, call := range calls {
		// One that fails or is refused stays out; T3's next turn finds it
		// missing. The server it is added to is still good.
		if err := p.add(ctx, up, path, call); err != nil {
			_, _ = fmt.Fprintf(p.stderr, "fern: MCP server %s was not added to opencode again: %v\n", path, err)
		}
	}
	events, err := p.openEvents(ctx, up)
	if err != nil {
		return fail(err)
	}
	up.events, up.br = events, bufio.NewReaderSize(events, 64<<10)
	// Its first event, server.connected, with a time limit of its own.
	late := time.AfterFunc(serverStartTimeout, cancel)
	up.first, err = readEvent(up.br)
	late.Stop()
	if err != nil {
		_ = events.Close()
		return fail(fmt.Errorf("opencode's events: %w", err))
	}
	return up, nil
}

// early says why a server that ended before it was in use did.
func (up *upstream) early() error {
	if up.err != nil {
		return fmt.Errorf("opencode: %w", up.err)
	}
	return fmt.Errorf("opencode exited with %d as it started", up.code)
}

// follow tells the actor up's events until they end.
func (p *proxy) follow(up *upstream) {
	defer func() { _ = up.events.Close() }()
	for {
		ev, err := readEvent(up.br)
		if err != nil {
			break
		}
		if !p.tell(func(context.Context) { p.event(up, ev) }) {
			return
		}
	}
	p.tell(func(context.Context) { p.eventsEnded(up) })
}
