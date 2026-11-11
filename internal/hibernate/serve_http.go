package hibernate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// The proxy's side towards T3 (its requests and event streams) and towards
// a server (the calls Serve makes itself). All of it runs outside the
// actor and tells it what happened (proxy.tell).

const (
	// heartbeat is how often T3's event stream gets a comment. T3 gives up on
	// a stream after 45 s of silence; OpenCode sends one every 15 s.
	heartbeat = 15 * time.Second
	// maxReplayed is the largest request body Serve keeps to send again.
	maxReplayed = 1 << 20
	// mcpPath is where T3 adds and removes its MCP servers.
	mcpPath = "/api/experimental/mcp/"
	// eventBacklog is how many events one of T3's streams may be behind
	// before it is closed.
	eventBacklog = 1024
)

// errEnded answers a request that came as Serve ended.
var errEnded = errors.New("the server is ending")

// ServeHTTP passes T3's requests on to the server, starting it if needed,
// and serves T3's event streams itself.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.auth != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(p.auth)) != 1 {
		// Refused here, so a stray request doesn't wake the box.
		w.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/event" {
		p.serveEvents(w, r)
		return
	}
	// T3's MCP servers are kept, to add them again to the next server.
	mcp := isMCPServer(r)
	var body []byte
	if mcp && r.Method == http.MethodPut {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxReplayed+1))
		if err != nil || len(b) > maxReplayed {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		body = b
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	}
	up, err := p.serverFor(r.Context())
	if err != nil {
		http.Error(w, "fern: opencode: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer p.tell(func(context.Context) { p.release() })
	if !mcp {
		up.pass.ServeHTTP(w, r)
		return
	}
	p.passTo(up.base, func(resp *http.Response) error {
		if resp.StatusCode/100 == 2 {
			p.keepMCP(r, body)
		}
		return nil
	}).ServeHTTP(w, r)
}

// passTo returns what passes requests on to the server at base, as they
// are, and its answers back as they come; seen, if not nil, sees each
// answer first.
func (p *proxy) passTo(base *url.URL, seen func(*http.Response) error) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:        func(pr *httputil.ProxyRequest) { pr.SetURL(base) },
		Transport:      p.transport,
		FlushInterval:  -1,
		ModifyResponse: seen,
		// The server went away under the request: its box stopped, or it died.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "fern: opencode: "+err.Error(), http.StatusBadGateway)
		},
	}
}

// isMCPServer says whether r adds or removes one of T3's MCP servers.
func isMCPServer(r *http.Request) bool {
	name, ok := strings.CutPrefix(r.URL.Path, mcpPath)
	return ok && name != "" && !strings.Contains(name, "/") && (r.Method == http.MethodPut || r.Method == http.MethodDelete)
}

// keepMCP tells the actor of an MCP server the server took or dropped.
func (p *proxy) keepMCP(r *http.Request, body []byte) {
	key := r.URL.Path + "?" + r.URL.RawQuery
	var call *resent
	if r.Method == http.MethodPut {
		call = &resent{header: http.Header{}, body: body}
		for _, k := range []string{"Authorization", "Content-Type"} {
			if v := r.Header.Get(k); v != "" {
				call.header.Set(k, v)
			}
		}
	}
	p.tell(func(context.Context) { p.setMCP(key, call) })
}

// serverFor returns the server for a request, once one is up, and has the
// request counted as under way: the caller ends that with release.
func (p *proxy) serverFor(ctx context.Context) (*upstream, error) {
	reply := make(chan grant, 1)
	// The server is started in the actor's context: it outlives the request.
	if !p.tell(func(ctx context.Context) { p.acquire(ctx, reply) }) {
		return nil, errEnded
	}
	select {
	case g := <-reply:
		return g.up, g.err
	case <-p.done:
		return nil, errEnded
	case <-ctx.Done():
	}
	// Gave up waiting. The actor answers, unless it did already: a server
	// granted meanwhile is given back.
	if !p.tell(func(context.Context) { p.abandon(reply, ctx.Err()) }) {
		return nil, errEnded
	}
	if g := <-reply; g.err == nil {
		p.tell(func(context.Context) { p.release() })
	}
	return nil, ctx.Err()
}

// serveEvents serves one of T3's event streams: the first server's
// server.connected, then every server's events, with a heartbeat between
// them. It ends when T3 closes it or its server ends by itself.
func (p *proxy) serveEvents(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, eventBacklog)
	first := make(chan []byte, 1)
	if !p.tell(func(context.Context) { p.subscribe(ch, first) }) {
		http.Error(w, "fern: opencode: "+errEnded.Error(), http.StatusBadGateway)
		return
	}
	// The actor may have closed and dropped ch by then.
	defer p.tell(func(context.Context) { p.unsubscribe(ch) })
	flusher, _ := w.(http.Flusher)
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
	if hello := <-first; hello != nil && !write(hello) {
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

// add sends one of T3's MCP servers to up.
func (p *proxy) add(ctx context.Context, up *upstream, path string, call resent) error {
	ctx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.base.String()+path, bytes.NewReader(call.body))
	if err != nil {
		return err
	}
	req.Header = call.header.Clone()
	resp, err := p.transport.RoundTrip(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errors.New(resp.Status)
	}
	return nil
}

// openEvents opens up's event stream.
func (p *proxy) openEvents(ctx context.Context, up *upstream) (io.ReadCloser, error) {
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

// sse is one event of a server's stream.
type sse struct {
	frame   []byte // as it came, with the blank line that ends it
	kind    string // its type, if it is one the actor acts on
	session string // the session it is about, then
}

// The events the actor acts on. The others, such as every piece of text a
// model writes, are passed on unread.
var acted = [][]byte{[]byte(`"session.execution.`), []byte(`"server.connected"`)}

// readEvent reads the next event of a stream. Comments (heartbeats) are
// skipped.
func readEvent(br *bufio.Reader) (sse, error) {
	var frame, data []byte
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return sse{}, err
		}
		if len(bytes.TrimRight(line, "\r\n")) > 0 {
			frame = append(frame, line...)
			if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				data = append(data, bytes.TrimSpace(v)...)
			}
			continue
		}
		if len(data) == 0 {
			frame = frame[:0]
			continue
		}
		ev := sse{frame: append(frame, '\n')}
		for _, mark := range acted {
			if !bytes.Contains(data, mark) {
				continue
			}
			var m struct {
				Type string `json:"type"`
				Data struct {
					SessionID string `json:"sessionID"`
				} `json:"data"`
			}
			if json.Unmarshal(data, &m) == nil {
				ev.kind, ev.session = m.Type, m.Data.SessionID
			}
			break
		}
		return ev, nil
	}
}
