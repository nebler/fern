package boxexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// MCPRelayPort is where an agent box's fern-init relays MCP to the daemon,
	// on the box's loopback.
	MCPRelayPort = 7072
	// MCPRelayPath is where fern-init says it has the relay, on Port.
	MCPRelayPath = "/mcp-relay"
	// BoxTokenHeader carries the box's token to the daemon on a call to T3's
	// MCP server, beside T3's own Authorization, which T3 needs.
	BoxTokenHeader = "X-Fern-Box"
	// CallKeyHeader names one call: the relay sends the same value each time.
	CallKeyHeader = "X-Fern-Call"
	// relayAuthHeader, in the answer to MCPRelayPath, says the relay adds the
	// box's token to every call itself, so Claude need not be given it.
	relayAuthHeader = "X-Fern-Relay-Auth"
	// maxMCPCall is the largest call the relay holds to send again.
	maxMCPCall = 16 << 20
)

// MCPRelay stands between Claude and the daemon's MCP server, in the agent
// box. The daemon is restarted to upgrade it, which breaks the connection of a
// tool call running then, and Claude would read that as the tool failing. The
// relay sends the call again until a daemon answers, under the same key, which
// is what lets the daemon find a command that call started (session.go).
//
// It also adds the box's token to every call, so the token is in fern-init's
// memory and never in Claude's arguments, where anything in the box could
// read it and a log could keep it.
//
// And it answers a tool call at once, as a stream that stays open until the
// daemon's answer is its one message. Claude Code gives the answer to a POST
// 60 s to begin, whatever the tool's own limit: the daemon answers when the
// tool is done, so every Bash command longer than a minute failed with "The
// operation timed out." (VPS, 2026-11-12).
type MCPRelay struct {
	daemon string
	token  string
	limit  time.Duration
	client *http.Client

	// mu guards calls: the tool calls under way, by their JSON-RPC ID, which
	// is how Claude names the one it has given up on.
	mu    sync.Mutex
	calls map[string]*relayCall
}

// relayCall is the tool calls under way with one ID: one, unless two clients
// in the box count alike.
type relayCall struct {
	n      int
	cancel context.CancelFunc // nil once there were two: the ID names neither
}

// relayKeepAlive is how often a tool call's stream gets a comment line while
// its tool runs, so that nothing takes a quiet stream for a dead one.
const relayKeepAlive = 15 * time.Second

// NewMCPRelay returns a relay to the daemon at URL daemon, which
// authenticates each call with the box's token and sends it again for up to
// limit.
func NewMCPRelay(daemon, token string, limit time.Duration) *MCPRelay {
	return &MCPRelay{daemon: daemon, token: token, limit: limit, client: &http.Client{}, calls: make(map[string]*relayCall)}
}

// rpcMessage is what the relay reads of a JSON-RPC message.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		RequestID json.RawMessage `json:"requestId"`
	} `json:"params"`
}

func (m *MCPRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPCall))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var msg rpcMessage
	_ = json.Unmarshal(body, &msg) // anything else is passed on as it is
	switch {
	case r.Method == http.MethodPost && msg.Method == "tools/call" && len(msg.ID) > 0:
		m.stream(w, r, body, msg.ID)
		return
	case msg.Method == "notifications/cancelled":
		m.cancelCall(string(msg.Params.RequestID))
	}
	resp, err := m.send(r.Context(), r, body)
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for name, values := range resp.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(flushing(w), resp.Body)
}

// send sends the call r, whose body is body, to the daemon until one
// answers, and returns the answer; an error if none does within the limit, or
// ctx is done first.
func (m *MCPRelay) send(ctx context.Context, r *http.Request, body []byte) (*http.Response, error) {
	key := rand.Text()
	wait := 50 * time.Millisecond
	// From the first attempt that fails, not from the call's start: the
	// daemon answers when the tool is done, which can be long after.
	var deadline time.Time
	for {
		req, err := http.NewRequestWithContext(ctx, r.Method, m.daemon+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = r.Header.Clone()
		req.Header.Set(CallKeyHeader, key)
		req.Header.Set("Authorization", "Bearer "+m.token)
		resp, err := m.client.Do(req)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // Claude has given up on the call.
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(m.limit)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the daemon doesn't answer: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// stream answers tool call id at once with an event stream, and sends the
// daemon's answer as its one message when the daemon has it. What the daemon
// can't answer becomes the call's JSON-RPC error: the stream's status has
// been sent by then.
func (m *MCPRelay) stream(w http.ResponseWriter, r *http.Request, body []byte, id json.RawMessage) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer m.track(string(id), cancel)()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	out := flushing(w)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	type answer struct {
		data []byte
		err  error
	}
	// It ends with send, which ends with ctx, and this function waits for it.
	done := make(chan answer, 1)
	go func() {
		data, err := m.answer(ctx, r, body)
		done <- answer{data, err}
	}()
	tick := time.NewTicker(relayKeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			_, _ = io.WriteString(out, ": the tool is running\n\n")
		case a := <-done:
			if ctx.Err() != nil {
				return // Claude has given up on the call, or gone.
			}
			if a.err != nil {
				a.data = rpcError(id, a.err)
			}
			_, _ = fmt.Fprintf(out, "event: message\ndata: %s\n\n", a.data)
			return
		}
	}
}

// answer returns the daemon's answer to the call as one line of JSON.
func (m *MCPRelay) answer(ctx context.Context, r *http.Request, body []byte) ([]byte, error) {
	resp, err := m.send(ctx, r, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMCPCall))
	if err != nil {
		return nil, fmt.Errorf("reading the daemon's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the daemon answered %s: %s", resp.Status, bytes.TrimSpace(data[:min(len(data), 512)]))
	}
	// One line: a line break would end the event's data.
	var line bytes.Buffer
	if err := json.Compact(&line, data); err != nil {
		return nil, fmt.Errorf("the daemon's answer isn't JSON: %w", err)
	}
	return line.Bytes(), nil
}

// rpcError returns call id's answer when it failed with err.
func rpcError(id json.RawMessage, err error) []byte {
	data, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32603, "message": err.Error()},
	})
	return data
}

// track records a tool call under way with JSON-RPC ID id, which cancel
// ends, and returns what forgets it.
func (m *MCPRelay) track(id string, cancel context.CancelFunc) (forget func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.calls[id]
	if c == nil {
		c = &relayCall{cancel: cancel}
		m.calls[id] = c
	} else {
		c.cancel = nil
	}
	c.n++
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if c.n--; c.n == 0 {
			delete(m.calls, id)
		}
	}
}

// cancelCall ends the tool call under way with JSON-RPC ID id, which Claude
// has given up on (notifications/cancelled): its command ends with the call's
// connection to the daemon. The stream Claude holds stays open until then, so
// without this a command Claude was interrupted in ran on to its own limit.
func (m *MCPRelay) cancelCall(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.calls[id]; c != nil && c.cancel != nil {
		c.cancel()
	}
}

// flushing passes on each write at once: an answer may be a stream.
func flushing(w http.ResponseWriter) io.Writer {
	f, ok := w.(http.Flusher)
	if !ok {
		return w
	}
	return flushWriter{w, f}
}

type flushWriter struct {
	io.Writer
	f http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.f.Flush()
	return n, err
}

// MCPRelayCheck answers that this box has the relay.
func MCPRelayCheck(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set(relayAuthHeader, "1")
		w.WriteHeader(http.StatusNoContent)
	})
}

// HasMCPRelay reports whether the box whose fern-init is at addr has the
// relay, and whether that relay adds the box's token itself: one in a box
// started before 2026-11-12 doesn't.
func HasMCPRelay(ctx context.Context, hc *http.Client, addr, token string) (relay, auth bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+MCPRelayPath, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return false, false
	}
	_ = resp.Body.Close()
	relay = resp.StatusCode == http.StatusNoContent
	return relay, relay && resp.Header.Get(relayAuthHeader) != ""
}
