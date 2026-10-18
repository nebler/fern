// Package boxexec is the exec protocol spoken by the CLI, the daemon and
// fern-init: one WebSocket per command.
//
// The client sends a JSON Request as a text message, then binary frames for
// stdin, stdin EOF and terminal resizes. The server sends binary frames for
// stdout and stderr, then the exit code as a JSON text message, and closes.
// Every binary frame starts with one byte saying what it carries.
package boxexec

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/creack/pty"
)

// Port is the port fern-init serves exec on inside every box.
const Port = 7070

// Frame kinds: the first byte of every binary message.
const (
	frameStdin byte = iota
	frameStdout
	frameStderr
	frameStdinEOF
	frameResize
)

// readLimit bounds one message; output frames are at most one pipe read.
const readLimit = 1 << 20

// Request starts a command. It is the first message on a connection.
type Request struct {
	Argv []string `json:"argv"`
	TTY  bool     `json:"tty,omitempty"`
	Size Size     `json:"size,omitzero"` // initial terminal size when TTY is set
}

// Size is a terminal size.
type Size struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// exit is the last message from the server.
type exit struct {
	Code int `json:"code"`
}

// Server runs commands for clients that present its token.
type Server struct {
	token string
	dir   string
	env   []string
	log   *slog.Logger
}

// NewServer returns a Server that runs commands in dir with environment env.
func NewServer(token, dir string, env []string, log *slog.Logger) *Server {
	return &Server{token: token, dir: dir, env: env, log: log}
}

// ServeHTTP runs one command over a WebSocket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := "Bearer " + s.token
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has answered the client.
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)
	var req Request
	if err := wsjson.Read(r.Context(), conn, &req); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	p, err := s.start(ctx, conn, req)
	if err != nil {
		fmt.Fprintf(frames(r.Context(), conn, frameStderr), "exec: %v\n", err)
		finish(r.Context(), conn, 127)
		return
	}
	defer p.close()
	// The reader stops when the connection closes. A client that goes away
	// cancels the command.
	var reader sync.WaitGroup
	reader.Go(func() {
		if readInput(r.Context(), conn, p) != nil {
			cancel()
		}
	})
	code := p.wait(s.log)
	finish(r.Context(), conn, code)
	reader.Wait()
}

// process is a running command.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	tty    *os.File // the terminal's controlling side; nil without a terminal
	output sync.WaitGroup
}

func (s *Server) start(ctx context.Context, conn *websocket.Conn, req Request) (*process, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("no command")
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir, cmd.Env = s.dir, s.env
	// Don't wait forever for output from background processes the command left behind.
	cmd.WaitDelay = 2 * time.Second
	p := &process{cmd: cmd}
	if !req.TTY {
		cmd.Stdout, cmd.Stderr = frames(ctx, conn, frameStdout), frames(ctx, conn, frameStderr)
		var err error
		if p.stdin, err = cmd.StdinPipe(); err != nil {
			return nil, err
		}
		return p, cmd.Start()
	}
	tty, err := pty.StartWithSize(cmd, winsize(req.Size))
	if err != nil {
		return nil, err
	}
	p.tty, p.stdin = tty, tty
	// Reading the terminal ends with EIO once the command and its children
	// have closed it.
	p.output.Go(func() { _, _ = io.Copy(frames(ctx, conn, frameStdout), tty) })
	return p, nil
}

// wait waits for the command and its output, and returns its exit code.
func (p *process) wait(log *slog.Logger) int {
	err := p.cmd.Wait()
	p.output.Wait()
	return exitCode(err, log)
}

func (p *process) close() {
	if p.tty != nil {
		_ = p.tty.Close()
	}
}

// finish sends the exit code and closes the connection.
func finish(ctx context.Context, conn *websocket.Conn, code int) {
	if wsjson.Write(ctx, conn, exit{Code: code}) == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	_ = conn.CloseNow()
}

// readInput copies stdin frames to the command and applies resizes until the
// connection fails.
func readInput(ctx context.Context, conn *websocket.Conn, p *process) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case frameStdin:
			_, _ = p.stdin.Write(data[1:]) // the command may have stopped reading
		case frameStdinEOF:
			if p.tty == nil { // a terminal has no EOF of its own; keep it open
				_ = p.stdin.Close()
			}
		case frameResize:
			if p.tty != nil && len(data) == 5 {
				size := Size{Rows: binary.BigEndian.Uint16(data[1:]), Cols: binary.BigEndian.Uint16(data[3:])}
				_ = pty.Setsize(p.tty, winsize(size))
			}
		}
	}
}

func exitCode(err error, log *slog.Logger) int {
	var exitErr *exec.ExitError
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		return 0
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	default:
		log.Warn("exec: wait", slog.Any("error", err))
		return 127
	}
}

func winsize(s Size) *pty.Winsize {
	if s.Rows == 0 || s.Cols == 0 {
		return &pty.Winsize{Rows: 24, Cols: 80}
	}
	return &pty.Winsize{Rows: s.Rows, Cols: s.Cols}
}

// frames returns a writer that sends what is written to it as frames of one
// kind.
func frames(ctx context.Context, conn *websocket.Conn, kind byte) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{kind}, p...)); err != nil {
			return 0, err
		}
		return len(p), nil
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Dial opens an exec connection to url, authenticating with token if it isn't
// empty. A nil hc means http.DefaultClient.
func Dial(ctx context.Context, url, token string, hc *http.Client) (*websocket.Conn, error) {
	opts := &websocket.DialOptions{HTTPClient: hc, HTTPHeader: http.Header{}}
	if token != "" {
		opts.HTTPHeader.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, url, opts)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(readLimit)
	return conn, nil
}

// Run starts req on conn, streams stdin to the command (unless stdin is nil)
// and its output to stdout and stderr, forwards terminal sizes from resize
// (which may be nil), and returns the command's exit code.
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
	}
	if resize != nil {
		resizer.Go(func() { sendResizes(ctx, conn, resize) })
	}
	for {
		typ, data, err := conn.Read(ctx)
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
	buf := make([]byte, 32<<10)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if conn.Write(ctx, websocket.MessageBinary, append([]byte{frameStdin}, buf[:n]...)) != nil {
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

// Proxy copies messages between a and b, both ways, until one side closes,
// and passes a normal close on to the other side.
func Proxy(ctx context.Context, a, b *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- forward(ctx, a, b) }()
	go func() { errc <- forward(ctx, b, a) }()
	err := <-errc
	cancel()
	<-errc
	return err
}

func forward(ctx context.Context, dst, src *websocket.Conn) error {
	for {
		typ, data, err := src.Read(ctx)
		if status := websocket.CloseStatus(err); status != -1 {
			_ = dst.Close(status, "")
			return nil
		}
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return err
		}
	}
}
