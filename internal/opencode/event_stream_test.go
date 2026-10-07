package opencode

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type eventTestBody struct {
	io.Reader
	closes atomic.Int32
}

func (b *eventTestBody) Close() error {
	b.closes.Add(1)
	return nil
}

func awaitEventWorker(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event worker did not complete")
	}
}

func TestAttachmentEventStreamFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"heartbeat", ": ping\r\n\r\n", ": ping\n\n"},
		{"global", "data: {\"type\":\"server.connected\"}\n\n", "data: {\"type\":\"server.connected\"}\n\n"},
		{"own", "data: {\"type\":\"session.updated\",\"id\":\"ses_own\"}\n\n", "data: {\"type\":\"session.updated\",\"id\":\"ses_own\"}\n\n"},
		{"foreign", "data: {\"type\":\"session.updated\",\"id\":\"ses_other\"}\n\n", ""},
		{"unfenced", "data: {\"type\":\"message.updated\"}\n\n", ""},
		{"nested foreign", "data: {\"items\":[{\"sessionID\":\"ses_own\"},{\"parentID\":\"ses_other\"}]}\n\n", ""},
		{"invalid", "data: not json\n\n", ""},
		{"multiline", "data: {\"type\":\"message.updated\",\n data ignored\ndata: \"session_id\":\"ses_own\"}\n\n", "data: {\"type\":\"message.updated\",\n data ignored\ndata: \"session_id\":\"ses_own\"}\n\n"},
		{"unterminated", "data: {}", "data: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &eventTestBody{Reader: strings.NewReader(tc.input)}
			stream := filterAttachmentEvents(body, "ses_own").(*attachmentEventStream)
			t.Cleanup(func() { _ = stream.Close() })
			got, err := io.ReadAll(stream)
			if err != nil || string(got) != tc.want {
				t.Fatalf("ReadAll = %q, %v; want %q", got, err, tc.want)
			}
			awaitEventWorker(t, stream.done)
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
			if body.closes.Load() != 1 {
				t.Fatalf("upstream closed %d times", body.closes.Load())
			}
		})
	}
}

func TestAttachmentEventStreamCumulativeLimit(t *testing.T) {
	// Each line is well below the scanner limit; only their sum exceeds it.
	line := ":" + strings.Repeat("x", 1022) + "\n"
	for _, delimiter := range []string{"", "\n"} {
		body := &eventTestBody{Reader: strings.NewReader(strings.Repeat(line, maxAttachmentProjectionBytes/len(line)+1) + delimiter)}
		stream := filterAttachmentEvents(body, "ses_own").(*attachmentEventStream)
		got, err := io.ReadAll(stream)
		if !errors.Is(err, errAttachmentEventTooLarge) || len(got) != 0 {
			t.Fatalf("oversized event produced %d bytes, %v", len(got), err)
		}
		awaitEventWorker(t, stream.done)
		_ = stream.Close()
		if body.closes.Load() != 1 {
			t.Fatalf("upstream closed %d times", body.closes.Load())
		}
	}
}

func TestAttachmentEventStreamLimitBoundary(t *testing.T) {
	line := ":" + strings.Repeat("x", 1022) + "\n"
	// Reserve one short line and the blank event delimiter so the normalized
	// event is exactly the projection limit, without any oversized single line.
	exact := strings.Repeat(line, maxAttachmentProjectionBytes/len(line)-1) + ":" + strings.Repeat("x", 1021) + "\n\n"
	for _, extra := range []string{"", "x"} {
		input := extra + exact
		stream := filterAttachmentEvents(io.NopCloser(strings.NewReader(input)), "ses_own")
		n, err := io.Copy(io.Discard, stream)
		_ = stream.Close()
		if extra == "" {
			if err != nil || n != maxAttachmentProjectionBytes {
				t.Fatalf("exact-limit event = %d bytes, %v", n, err)
			}
		} else if !errors.Is(err, errAttachmentEventTooLarge) || n != 0 {
			t.Fatalf("limit+1 event = %d bytes, %v", n, err)
		}
	}
}

type blockedEventBody struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (b *blockedEventBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *blockedEventBody) Close() error {
	if b.closes.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}

func TestAttachmentEventStreamCloseBlockedRead(t *testing.T) {
	body := &blockedEventBody{entered: make(chan struct{}), closed: make(chan struct{})}
	stream := filterAttachmentEvents(body, "ses_own").(*attachmentEventStream)
	awaitEventWorker(t, body.entered)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = stream.Close() }()
		}
		wg.Wait()
	}()
	awaitEventWorker(t, closed)
	select {
	case <-stream.done:
	default:
		t.Fatal("Close returned before worker completion")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("upstream closed %d times", body.closes.Load())
	}
	if _, err := stream.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Read after Close = %v", err)
	}
}

func TestAttachmentEventStreamCloseWithoutConsumer(t *testing.T) {
	body := &eventTestBody{Reader: strings.NewReader(": heartbeat\n\n")}
	stream := filterAttachmentEvents(body, "ses_own").(*attachmentEventStream)
	closed := make(chan struct{})
	go func() { _ = stream.Close(); close(closed) }()
	awaitEventWorker(t, closed)
	awaitEventWorker(t, stream.done)
}

func TestAttachmentEventStreamLimitResets(t *testing.T) {
	// The stream can exceed the limit in total when individual events do not.
	event := ":" + strings.Repeat("x", (64<<10)-3) + "\n\n"
	input := strings.Repeat(event, maxAttachmentProjectionBytes/len(event)+1)
	stream := filterAttachmentEvents(io.NopCloser(strings.NewReader(input)), "ses_own")
	defer stream.Close()
	n, err := io.Copy(io.Discard, stream)
	if err != nil || n != int64(len(input)) {
		t.Fatalf("Copy = %d, %v; want %d bytes", n, err, len(input))
	}
}
