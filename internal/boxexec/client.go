package boxexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// DialError is a handshake the server refused with an HTTP answer.
type DialError struct {
	StatusCode int
	Body       []byte // the start of the answer's body
	Err        error
}

func (e *DialError) Error() string { return e.Err.Error() }

func (e *DialError) Unwrap() error { return e.Err }

// Dial opens an exec connection to url, authenticating with token if it isn't
// empty. A nil hc means http.DefaultClient. If the server answers the
// handshake with an error, the error is a *DialError.
func Dial(ctx context.Context, url, token string, hc *http.Client) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{HTTPClient: hc, HTTPHeader: http.Header{}}
	if token != "" {
		opts.HTTPHeader.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, url, opts)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			// The library has already read the start of the body into memory.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			return nil, &DialError{StatusCode: resp.StatusCode, Body: body, Err: err}
		}
		return nil, err
	}
	conn.SetReadLimit(MaxMessage)
	return conn, nil
}

// Run starts req on conn, streams stdin to the command and its output to
// stdout and stderr, forwards terminal sizes from resize (which may be nil),
// and returns the command's exit code. A nil stdin is empty: the command
// reads EOF at once.
//
// A Read on stdin that blocks when the command exits can't be interrupted;
// the goroutine reading it ends at its next Read.
func Run(ctx context.Context, conn *websocket.Conn, req Request, stdin io.Reader,
	stdout, stderr io.Writer, resize <-chan Size,
) (int, error) {
	if err := wsjson.Write(ctx, conn, req); err != nil {
		return 0, fmt.Errorf("exec: send request: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	var resizer sync.WaitGroup
	defer resizer.Wait()
	defer cancel()
	if stdin != nil {
		go sendStdin(ctx, conn, stdin)
	} else if err := conn.Write(ctx, websocket.MessageBinary, []byte{frameStdinEOF}); err != nil {
		return 0, fmt.Errorf("exec: close stdin: %w", err)
	}
	if resize != nil {
		resizer.Go(func() { sendResizes(ctx, conn, resize) })
	}
	r := &messageReader{conn: conn}
	for {
		typ, data, err := r.read(ctx)
		if err != nil {
			return 0, fmt.Errorf("exec: %w", err)
		}
		if typ == websocket.MessageText {
			var e exit
			if err := json.Unmarshal(data, &e); err != nil {
				return 0, fmt.Errorf("exec: bad exit message: %w", err)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return e.Code, nil
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case frameStdout:
			_, err = stdout.Write(data[1:])
		case frameStderr:
			_, err = stderr.Write(data[1:])
		}
		if err != nil {
			return 0, fmt.Errorf("exec: write output: %w", err)
		}
	}
}

func sendStdin(ctx context.Context, conn *websocket.Conn, stdin io.Reader) {
	// Reading into the frame itself saves a copy and an allocation each.
	buf := make([]byte, 1+frameData)
	buf[0] = frameStdin
	for {
		n, err := stdin.Read(buf[1:])
		if n > 0 {
			if conn.Write(ctx, websocket.MessageBinary, buf[:1+n]) != nil {
				return
			}
		}
		if err != nil {
			_ = conn.Write(ctx, websocket.MessageBinary, []byte{frameStdinEOF})
			return
		}
	}
}

func sendResizes(ctx context.Context, conn *websocket.Conn, resize <-chan Size) {
	for {
		select {
		case <-ctx.Done():
			return
		case size := <-resize:
			msg := []byte{frameResize, 0, 0, 0, 0}
			binary.BigEndian.PutUint16(msg[1:], size.Rows)
			binary.BigEndian.PutUint16(msg[3:], size.Cols)
			if conn.Write(ctx, websocket.MessageBinary, msg) != nil {
				return
			}
		}
	}
}
