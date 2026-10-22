package boxexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"syscall"

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
			body, _ := io.ReadAll(resp.Body)
			return nil, &DialError{StatusCode: resp.StatusCode, Body: body, Err: err}
		}
		return nil, err
	}
	conn.SetReadLimit(maxMessage)
	return conn, nil
}

// Run starts req on conn, connects it to s, and returns the command's exit
// code.
//
// A Read on s.Stdin that blocks when the command exits can't be interrupted;
// the goroutine reading it ends at its next Read.
func Run(ctx context.Context, conn *websocket.Conn, req Request, s Streams) (int, error) {
	if err := wsjson.Write(ctx, conn, req); err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	var senders sync.WaitGroup
	defer senders.Wait()
	defer cancel()
	if s.Stdin != nil {
		go sendStdin(ctx, conn, s.Stdin)
	} else if err := conn.Write(ctx, websocket.MessageBinary, []byte{frameStdinEOF}); err != nil {
		return 0, fmt.Errorf("close stdin: %w", err)
	}
	senders.Go(func() { sendControls(ctx, conn, s.Resize, s.Signal) })
	r := &messageReader{conn: conn}
	for {
		typ, data, err := r.read(ctx)
		if err != nil {
			return 0, err
		}
		if typ == websocket.MessageText {
			var e exit
			if err := json.Unmarshal(data, &e); err != nil {
				return 0, fmt.Errorf("bad exit message: %w", err)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return e.Code, nil
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case frameStdout:
			_, err = s.Stdout.Write(data[1:])
		case frameStderr:
			_, err = s.Stderr.Write(data[1:])
		}
		if err != nil {
			return 0, fmt.Errorf("write output: %w", err)
		}
	}
}

// sendStdin sends stdin until it ends, then its end.
func sendStdin(ctx context.Context, conn *websocket.Conn, stdin io.Reader) {
	_, _ = frames(ctx, conn, frameStdin).ReadFrom(stdin)
	_ = conn.Write(ctx, websocket.MessageBinary, []byte{frameStdinEOF})
}

// sendControls sends terminal sizes and signals; either channel may be nil.
func sendControls(ctx context.Context, conn *websocket.Conn, resize <-chan Size, sigs <-chan os.Signal) {
	for {
		var msg []byte
		select {
		case <-ctx.Done():
			return
		case size := <-resize:
			msg = []byte{frameResize, 0, 0, 0, 0}
			binary.BigEndian.PutUint16(msg[1:], size.Rows)
			binary.BigEndian.PutUint16(msg[3:], size.Cols)
		case sig := <-sigs:
			s, ok := sig.(syscall.Signal)
			if !ok {
				continue
			}
			msg = []byte{frameSignal, byte(s)}
		}
		if conn.Write(ctx, websocket.MessageBinary, msg) != nil {
			return
		}
	}
}
