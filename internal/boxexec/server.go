package boxexec

import (
	"bytes"
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
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// outputDelay is how long a command's output may go on after it exits: a
// process it moved out of its group can hold its pipes or terminal open.
const outputDelay = 2 * time.Second

// exitNotStarted is the exit code of a command that couldn't start, as a
// shell reports one it can't find.
const exitNotStarted = 127

// forwardable reports whether a client may send sig. Others would let a
// client do more than ask the command it started to stop.
func forwardable(sig syscall.Signal) bool {
	return sig == syscall.SIGHUP || sig == syscall.SIGINT || sig == syscall.SIGTERM
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
		_ = conn.Write(r.Context(), websocket.MessageBinary, fmt.Appendf([]byte{frameStderr}, "exec: %v\n", err))
		finish(r.Context(), conn, exitNotStarted)
		return
	}
	// The reader stops when the connection closes. A client that goes away
	// cancels the command. Stdin has a writer of its own, so that a command
	// that stops reading it doesn't hold up the signals and the close behind
	// it.
	stdin := make(chan []byte, stdinQueue)
	var streams sync.WaitGroup
	streams.Go(func() { writeStdin(p, stdin) })
	streams.Go(func() {
		readInput(ctx, conn, p, stdin)
		close(stdin)
		cancel()
	})
	code := p.wait(s.log)
	finish(r.Context(), conn, code)
	streams.Wait()
}

// stdinQueue is how many stdin frames, of up to frameData bytes each, wait
// for a command that isn't reading them before the client is held up.
const stdinQueue = 16

// Accept upgrades r to an exec connection, which reads larger messages than
// the WebSocket library's default.
func Accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxMessage)
	return conn, nil
}

// process is a running command.
type process struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	tty   *os.File // the terminal's controlling side; nil without a terminal
	// outputs are what the command's output is read from: the terminal, or
	// the pipes of stdout and stderr.
	outputs []*os.File
	output  sync.WaitGroup
	exited  atomic.Bool // the command has been reaped: don't signal its group
	trace   *slog.Logger
	start   time.Time
}

func (s *Server) start(ctx context.Context, conn *websocket.Conn, req Request) (*process, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("no command")
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir, cmd.Env = s.dir, s.env
	if req.Dir != "" {
		cmd.Dir = req.Dir
	}
	if len(req.Environ) > 0 {
		// Later entries win (os/exec keeps the last of each name).
		cmd.Env = append(slices.Clone(s.env), req.Environ...)
	}
	// The command leads its own process group (a terminal makes it a session
	// leader, which is one too). Cancelling kills the whole group, so nothing
	// the command started keeps writing to the workspace after the client
	// has gone.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	p := &process{cmd: cmd}
	// Only opted-in Claude launches are traced. Arguments and environment
	// values can contain credentials, so neither belongs in the trace.
	if filepath.Base(req.Argv[0]) == "claude" && slices.Contains(req.Environ, "FERN_TIMING=1") {
		p.trace = s.log.With(slog.String("dir", cmd.Dir))
		p.start = time.Now()
	}
	if !req.TTY {
		if err := p.startPipes(cmd); err != nil {
			return nil, err
		}
		p.copyOutput(ctx, conn, frameStdout, frameStderr)
		return p, nil
	}
	tty, err := startTTY(cmd, req.Size)
	if err != nil {
		return nil, err
	}
	p.tty, p.stdin, p.outputs = tty, tty, []*os.File{tty}
	p.copyOutput(ctx, conn, frameStdout)
	return p, nil
}

// startPipes starts cmd in a process group of its own, with pipes for its
// standard streams.
//
// The output pipes are the server's, not os/exec's: Wait would wait for
// os/exec's to be drained, and a background process the command left behind
// holds them open, so every such command would take outputDelay to end.
// With its own, the server can kill the group as soon as the command exits,
// which closes them.
func (p *process) startPipes(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var writers []*os.File
	defer func() {
		for _, w := range writers {
			_ = w.Close() // the command has its own copies
		}
	}()
	for range 2 {
		r, w, err := os.Pipe()
		if err != nil {
			p.closeOutputs()
			return err
		}
		p.outputs, writers = append(p.outputs, r), append(writers, w)
	}
	cmd.Stdout, cmd.Stderr = writers[0], writers[1]
	var err error
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		p.closeOutputs()
		return err
	}
	// A failed Start closes the stdin pipe.
	if err := cmd.Start(); err != nil {
		p.closeOutputs()
		return err
	}
	return nil
}

// copyOutput sends what comes out of each of the outputs as frames of the
// matching kind. Each copy ends once the command and its children have
// closed their end (EOF; EIO for a terminal), or at the deadline wait sets.
func (p *process) copyOutput(ctx context.Context, conn *websocket.Conn, kinds ...byte) {
	if p.trace != nil {
		p.trace.InfoContext(ctx, "claude started",
			slog.Int("pid", p.cmd.Process.Pid),
			slog.Float64("spawn_ms", float64(time.Since(p.start))/float64(time.Millisecond)))
	}
	for i, out := range p.outputs {
		p.output.Go(func() {
			w := frames(ctx, conn, kinds[i])
			if p.trace != nil && kinds[i] == frameStdout {
				first := true
				w.send = func(frame []byte) error {
					if first {
						p.trace.InfoContext(ctx, "claude first stdout",
							slog.Int("pid", p.cmd.Process.Pid),
							slog.Float64("since_request_ms", float64(time.Since(p.start))/float64(time.Millisecond)))
						first = false
					}
					return conn.Write(ctx, websocket.MessageBinary, frame)
				}
			}
			_, _ = io.Copy(w, out)
		})
	}
}

func (p *process) closeOutputs() {
	for _, out := range p.outputs {
		_ = out.Close()
	}
}

// startTTY starts cmd on a new terminal of the given size and returns the
// terminal's controlling side.
//
// The controlling side is in non-blocking mode, owned by Go's poller, so
// that a read of it can end at a deadline or when it is closed. pty.Open
// leaves it blocking (its ioctls go through Fd), and a blocking read only
// ends once every process holding the terminal has closed it, which one in
// another session may never do.
func startTTY(cmd *exec.Cmd, size Size) (*os.File, error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tty.Close() }() // the command has its own copy
	f, err := pollable(ptmx)
	if err != nil {
		return nil, err
	}
	if err := setSize(f, size); err != nil {
		_ = f.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	// Ctty is the command's stdin.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// pollable returns a non-blocking copy of f that Go's poller owns, and
// closes f. f is blocking already, so using its Fd changes nothing; the copy
// must never be touched through Fd, which would make it blocking again.
func pollable(f *os.File) (*os.File, error) {
	defer func() { _ = f.Close() }()
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("dup %s: %w", f.Name(), err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("set %s non-blocking: %w", f.Name(), err)
	}
	// NewFile sees O_NONBLOCK and hands the descriptor to the poller.
	return os.NewFile(uintptr(fd), f.Name()), nil
}

// setSize sets the size of the terminal whose controlling side is f.
func setSize(f *os.File, s Size) error {
	if s.Rows == 0 || s.Cols == 0 {
		s = Size{Rows: 24, Cols: 80}
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := rc.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: s.Rows, Col: s.Cols})
	}); err != nil {
		return err
	}
	return ioctlErr
}

// wait waits for the command and its output, closes the outputs, and
// returns the command's exit code.
func (p *process) wait(log *slog.Logger) int {
	err := p.cmd.Wait()
	p.exited.Store(true)
	// Kill what is left of the command's process group: its background
	// processes end with it, as T3's do when an agent exits. Without this they
	// would go on changing the workspace with nobody watching. Processes the
	// command moved to another group or session are out of reach.
	//
	// The group's ID is the leader's PID, which reaping has released. That is
	// safe from PID reuse: while any process is left in the group, the kernel
	// won't hand its ID out as a new PID, so the signal reaches only the
	// command's leftovers. Once the group is empty the ID may be reused, but a
	// new process would have to get it and make itself a group leader in the
	// moment since the reap to be hit; otherwise the kill finds no group.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL) // ESRCH: nothing was left
	// The kill closes what the group held open. A process it can't reach can
	// hold an output open; it gets outputDelay. Closing the terminal also ends
	// a write of stdin the command never read (Wait closes the stdin pipe).
	deadline := time.Now().Add(outputDelay)
	for _, out := range p.outputs {
		_ = out.SetReadDeadline(deadline)
	}
	p.output.Wait()
	p.closeOutputs()
	return exitCode(err, log)
}

// finish sends the exit code and closes the connection.
func finish(ctx context.Context, conn *websocket.Conn, code int) {
	if wsjson.Write(ctx, conn, exit{Code: code}) == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	_ = conn.CloseNow()
}

// readInput applies what the client sends (terminal sizes, signals) until
// the connection closes, and queues stdin and its end (nil) for writeStdin.
// Only a full queue holds it up: then the client waits for the command to
// read.
func readInput(ctx context.Context, conn *websocket.Conn, p *process, stdin chan<- []byte) {
	r := &messageReader{conn: conn}
	for {
		_, data, err := r.read(ctx)
		if err != nil {
			return
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case frameStdin:
			if len(data) > 1 && !queue(ctx, stdin, bytes.Clone(data[1:])) {
				return
			}
		case frameStdinEOF:
			// A terminal has no EOF of its own; keep it open.
			if p.tty == nil && !queue(ctx, stdin, nil) {
				return
			}
		case frameResize:
			if p.tty != nil && len(data) == 5 {
				size := Size{Rows: binary.BigEndian.Uint16(data[1:]), Cols: binary.BigEndian.Uint16(data[3:])}
				_ = setSize(p.tty, size) // fails once wait has closed the terminal
			}
		case frameSignal:
			// To the whole group, as a terminal would send it. Once the
			// command has been reaped its group id may be reused.
			if sig := syscall.Signal(data[len(data)-1]); len(data) == 2 && forwardable(sig) && !p.exited.Load() {
				_ = syscall.Kill(-p.cmd.Process.Pid, sig)
			}
		}
	}
}

// queue puts data on stdin, unless ctx ends first.
func queue(ctx context.Context, stdin chan<- []byte, data []byte) bool {
	select {
	case stdin <- data:
		return true
	case <-ctx.Done():
		return false
	}
}

// writeStdin writes what readInput queues to the command's stdin, and closes
// it at nil. After that, or a failed write (the command closed its stdin or
// exited), it drops the rest, so readInput is never held up.
func writeStdin(p *process, stdin <-chan []byte) {
	done := false
	for data := range stdin {
		switch {
		case done:
		case data == nil:
			_ = p.stdin.Close()
			done = true
		default:
			_, err := p.stdin.Write(data)
			done = err != nil
		}
	}
}

func exitCode(err error, log *slog.Logger) int {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	default:
		log.Warn("exec: wait", slog.Any("error", err))
		return exitNotStarted
	}
}
