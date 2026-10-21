package boxexec

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
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

// outputDelay is how long a command's output may go on after it exits:
// background processes it left behind can hold its pipes or terminal open.
const outputDelay = 2 * time.Second

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
	conn, err := Accept(w, r)
	if err != nil {
		return // Accept has answered the client.
	}
	defer func() { _ = conn.CloseNow() }()
	var req Request
	if err := wsjson.Read(r.Context(), conn, &req); err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	p, err := s.start(ctx, conn, req)
	if err != nil {
		_, _ = fmt.Fprintf(frames(r.Context(), conn, frameStderr), "exec: %v\n", err)
		finish(r.Context(), conn, 127)
		return
	}
	defer p.close()
	// The reader stops when the connection closes. A client that goes away
	// cancels the command.
	var reader sync.WaitGroup
	reader.Go(func() {
		readInput(ctx, conn, p)
		cancel()
	})
	code := p.wait(s.log)
	finish(r.Context(), conn, code)
	reader.Wait()
}

// Accept upgrades r to an exec connection that can read MaxMessage.
func Accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(MaxMessage)
	return conn, nil
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
	// The command leads its own process group (a terminal makes it a session
	// leader, which is one too). Cancelling kills the whole group, so nothing
	// the command started keeps writing to the workspace after the client
	// has gone.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = outputDelay
	p := &process{cmd: cmd}
	if !req.TTY {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	killGroup(p.cmd.Process.Pid)
	if p.tty != nil {
		// cmd.WaitDelay only bounds pipes. A blocked read of the terminal
		// can't be interrupted, so stop what still holds it open: the rest
		// of the command's process group.
		t := time.AfterFunc(outputDelay, func() { _ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL) })
		defer t.Stop()
	}
	p.output.Wait()
	return exitCode(err, log)
}

// killGroup kills what is left of the process group whose leader was pid,
// once the leader has exited and been reaped: a command's background
// processes end with it, as T3's do when an agent exits. Without it they
// would go on changing the workspace with nobody watching. Processes the
// command moved to another group or session are out of reach.
//
// The group's ID is the leader's PID, which reaping has released. That is
// safe from PID reuse: while any process is left in the group, the kernel
// won't hand its ID out as a new PID, so the signal reaches only the
// command's leftovers. Once the group is empty the ID may be reused, but a
// new process would have to get it and make itself a group leader in the
// moment since the reap to be hit; otherwise the kill finds no group.
func killGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL) // ESRCH: nothing was left
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
// connection closes.
func readInput(ctx context.Context, conn *websocket.Conn, p *process) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
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
