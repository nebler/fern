package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

// Shared stream timing and framing budgets. Used by Stream/StreamForever and
// by StreamController so reconnect behavior cannot drift between the two.
const (
	// initialBackoff is the first reconnect delay after a failed connection.
	initialBackoff = 500 * time.Millisecond
	// maxBackoffReset is the connected duration after which backoff resets to
	// initialBackoff instead of continuing to grow.
	maxBackoffReset = 30 * time.Second
	// maxFrameBytes bounds one decoded SSE frame (all data lines joined).
	maxFrameBytes = 4 << 20 // 4 MiB
	// connectTimeout bounds a single connection setup attempt.
	connectTimeout = 10 * time.Second
	// drainTimeout bounds post-cancellation waits on goroutines already told
	// to stop.
	drainTimeout = 5 * time.Second
)

// errStreamClosed reports a clean server-side close of an otherwise healthy
// event stream. StreamForever treats it as expected behavior (Info) rather
// than a failure (Warn).
var errStreamClosed = errors.New("event stream closed")

// Event is one decoded SSE frame from the OpenCode event feed. Properties
// carries the V1 envelope payload field and Data the V2 field; consumers
// fall back between them per endpoint version.
type Event struct {
	ID         string          `json:"id,omitempty"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
	Data       json.RawMessage `json:"data"`
}

// StreamOptions configures one event-stream connection.
type StreamOptions struct {
	// BaseURL is the OpenCode origin; /api/event is appended.
	BaseURL string
	// Auth carries optional basic-auth credentials.
	Auth runtime.ServerAuth
	// Client optionally overrides the HTTP client; nil selects a client with
	// a bounded response-header timeout.
	Client *http.Client
	// OnConnect, when set, runs once per successful upgrade before frames
	// are decoded.
	OnConnect func()
}

var defaultStreamClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 10 * time.Second
	return &http.Client{Transport: transport}
}()

// Stream parses complete SSE frames. Multiple data lines belong to one frame
// and are joined according to the SSE specification before JSON decoding.
func Stream(ctx context.Context, options StreamOptions, out chan<- Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(options.BaseURL, "/")+"/api/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	options.Auth.Apply(req)
	client := options.Client
	if client == nil {
		client = defaultStreamClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream returned %s", resp.Status)
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])); mediaType != "text/event-stream" {
		return fmt.Errorf("event stream returned content type %q", resp.Header.Get("Content-Type"))
	}
	if options.OnConnect != nil {
		options.OnConnect()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxFrameBytes)
	var data []string
	frameBytes := 0
	emit := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		frameBytes = 0
		var event Event
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("decode SSE event: %w", err)
		}
		if event.Type == "" {
			return fmt.Errorf("decode SSE event: missing type")
		}
		select {
		case out <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := emit(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			frameBytes += len(value) + 1
			if frameBytes > maxFrameBytes {
				return fmt.Errorf("SSE frame exceeds %d bytes", maxFrameBytes)
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errStreamClosed
}

// StreamForever reconnects Stream until ctx is done. Reconnect delay doubles
// from initialBackoff up to a 15s cap and resets after any connection that
// survived maxBackoffReset. A graceful peer close (errStreamClosed) logs at
// Info; every other stream termination logs at Warn. It is used by the
// diagnostic command; lifecycle observation uses StreamController, which also
// publishes connection epochs.
func StreamForever(ctx context.Context, options StreamOptions, out chan<- Event, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	backoff := initialBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := Stream(ctx, options, out)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errStreamClosed) {
			log.Info("event stream closed", "connected_for", time.Since(start).Round(time.Millisecond))
		} else {
			log.Warn("event stream ended", "err", err, "connected_for", time.Since(start).Round(time.Millisecond))
		}
		if time.Since(start) > maxBackoffReset {
			backoff = initialBackoff
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func nextBackoff(backoff time.Duration) time.Duration {
	if backoff >= 15*time.Second {
		return 15 * time.Second
	}
	backoff *= 2
	if backoff > 15*time.Second {
		return 15 * time.Second
	}
	return backoff
}
