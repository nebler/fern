package opencode

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"
)

var errAttachmentEventTooLarge = errors.New("attachment event exceeds limit")

// attachmentEventStream owns the upstream body and the filtering worker. As with
// an HTTP response body, upstream.Close must interrupt an outstanding Read.
type attachmentEventStream struct {
	reader *io.PipeReader
	body   io.ReadCloser
	done   chan struct{}
	once   sync.Once
	err    error
}

func filterAttachmentEvents(body io.ReadCloser, sessionID string) io.ReadCloser {
	reader, writer := io.Pipe()
	stream := &attachmentEventStream{reader: reader, body: body, done: make(chan struct{})}
	go func() {
		defer close(stream.done)
		err := stream.filter(writer, sessionID)
		stream.closeUpstream()
		_ = writer.CloseWithError(err)
	}()
	return stream
}

func (s *attachmentEventStream) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

func (s *attachmentEventStream) closeUpstream() {
	s.once.Do(func() { s.err = s.body.Close() })
}

func (s *attachmentEventStream) Close() error {
	// Release a blocked pipe write before waiting for either upstream or worker.
	_ = s.reader.Close()
	s.closeUpstream()
	<-s.done
	return s.err
}

func (s *attachmentEventStream) filter(writer io.Writer, sessionID string) error {
	scanner := bufio.NewScanner(s.body)
	scanner.Buffer(make([]byte, 64<<10), maxAttachmentProjectionBytes)
	var event bytes.Buffer
	flush := func() error {
		if event.Len() != 0 && attachmentEventAllowed(event.Bytes(), sessionID) {
			if _, err := writer.Write(event.Bytes()); err != nil {
				return err
			}
		}
		event.Reset()
		return nil
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		// Include every line and its normalized newline, including comments and
		// the event delimiter. Check before appending, not only before flushing.
		if len(line)+1 > maxAttachmentProjectionBytes-event.Len() {
			return errAttachmentEventTooLarge
		}
		event.Write(line)
		event.WriteByte('\n')
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return scanner.Err()
}
