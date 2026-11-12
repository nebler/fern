package boxexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
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
type MCPRelay struct {
	daemon string
	token  string
	limit  time.Duration
	client *http.Client
}

// NewMCPRelay returns a relay to the daemon at URL daemon, which
// authenticates each call with the box's token and sends it again for up to
// limit.
func NewMCPRelay(daemon, token string, limit time.Duration) *MCPRelay {
	return &MCPRelay{daemon: daemon, token: token, limit: limit, client: &http.Client{}}
}

func (m *MCPRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPCall))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	key := rand.Text()
	wait := 50 * time.Millisecond
	// From the first attempt that fails, not from the call's start: the
	// daemon answers when the tool is done, which can be long after.
	var deadline time.Time
	for {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, m.daemon+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		req.Header.Set(CallKeyHeader, key)
		req.Header.Set("Authorization", "Bearer "+m.token)
		resp, err := m.client.Do(req)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			for name, values := range resp.Header {
				w.Header()[name] = values
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(flushing(w), resp.Body)
			return
		}
		if errors.Is(r.Context().Err(), context.Canceled) {
			return // Claude has given up on the call.
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(m.limit)
		}
		if time.Now().After(deadline) {
			http.Error(w, "the daemon doesn't answer: "+err.Error(), http.StatusBadGateway)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
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
