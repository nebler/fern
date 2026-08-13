package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/noah/fern/internal/runtime"
)

type Event struct {
	ID         string          `json:"id,omitempty"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

type StreamOptions struct {
	BaseURL   string
	Auth      runtime.ServerAuth
	Client    *http.Client
	OnConnect func()
}

// Stream parses complete SSE frames. Multiple data lines belong to one frame
// and are joined according to the SSE specification before JSON decoding.
func Stream(ctx context.Context, options StreamOptions, out chan<- Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(options.BaseURL, "/")+"/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	options.Auth.Apply(req)
	client := options.Client
	if client == nil {
		client = http.DefaultClient
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
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
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
			if frameBytes > 4*1024*1024 {
				return fmt.Errorf("SSE frame exceeds 4 MiB")
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("event stream closed")
}

// StreamForever is used by the diagnostic command. Lifecycle observation uses
// StreamController, which also publishes connection epochs.
func StreamForever(ctx context.Context, options StreamOptions, out chan<- Event, log *slog.Logger) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		start := time.Now()
		err := Stream(ctx, options, out)
		if ctx.Err() != nil {
			return
		}
		log.Warn("event stream ended", "err", err, "connected_for", time.Since(start).Round(time.Millisecond))
		if time.Since(start) > 30*time.Second {
			backoff = 500 * time.Millisecond
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
