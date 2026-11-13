package boxexec

import (
	"bytes"
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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// OutputDelay is how long a command's output may go on after it exits: a
// process it moved out of its group can hold its pipes or terminal open. A
// box's background commands get the same (package background).
const OutputDelay = 2 * time.Second

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

	// Grace is how long a session's command runs with no connection.
	Grace time.Duration

	mu       sync.Mutex
	sessions map[string]*session
	finished map[string]int // exit codes of sessions that ended lately
	activity Activity
}

// ActivityPath is where fern-init says when its box was last used, which is
// how the idle clocks of a box outlive the daemon that was keeping them.
const ActivityPath = "/activity"

// Activity is how a box has been used.
type Activity struct {
	Opened  time.Time `json:"opened"`  // a command last started
	Closed  time.Time `json:"closed"`  // a command last ended
	Running int       `json:"running"` // commands running, with a connection or waiting for one
}

// Opened records that a command has started. Background commands, which
// aren't this server's, count too.
func (s *Server) Opened() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activity.Opened = time.Now()
}

func (s *Server) running(by int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activity.Running += by
	if by > 0 {
		s.activity.Opened = time.Now()
	} else {
		s.activity.Closed = time.Now()
	}
}

// Authorized reports whether r carries token as its bearer token, which
// the daemon sends with every request to a box's fern-init; one that
// doesn't is answered here, with 401. The comparison takes the same time
// whatever the request sent, so it tells nothing of the token.
func Authorized(w http.ResponseWriter, r *http.Request, token string) bool {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// ServeActivity answers with the box's Activity.
func (s *Server) ServeActivity(w http.ResponseWriter, r *http.Request) {
	if !Authorized(w, r, s.token) {
		return
	}
	s.mu.Lock()
	a := s.activity
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

// ActivityOf returns the Activity of the box whose fern-init is at addr. A
// box from before it has none, an error.
func ActivityOf(ctx context.Context, hc *http.Client, addr, token string) (Activity, error) {
	var a Activity
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+ActivityPath, nil)
	if err != nil {
		return a, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return a, fmt.Errorf("activity: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return a, fmt.Errorf("activity: %s", resp.Status)
	}
	return a, json.NewDecoder(resp.Body).Decode(&a)
}

// NewServer returns a Server that runs commands in dir with environment env.
func NewServer(token, dir string, env []string, log *slog.Logger) *Server {
	now := time.Now()
	return &Server{token: token, dir: dir, env: env, log: log, Grace: DefaultGrace, activity: Activity{Opened: now, Closed: now}}
}

// ServeHTTP runs one command over a WebSocket.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !Authorized(w, r, s.token) {
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

	if req.Session != "" {
		s.serveSession(r.Context(), conn, req)
		return
	}

	s.running(1)
	defer s.running(-1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	p, err := s.start(ctx, req, func(kind byte) *frameWriter { return frames(ctx, conn, kind) })
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
	streams.Go(func() { writeStdin(p, stdin, nil) })
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

// start starts req's command, sending its output through the writers output
// makes, one per kind of frame.
func (s *Server) start(ctx context.Context, req Request, output func(kind byte) *frameWriter) (*process, error) {
	if len(req.Argv) == 0 {
		return nil, errors.New("no command")
	}
	env := s.env
	if len(req.Environ) > 0 {
		// Later entries win (os/exec keeps the last of each name).
		env = append(slices.Clone(s.env), req.Environ...)
	}
	// The program is found on the PATH it runs with, which puts the tools a
	// workspace pins first, not on this process's.
	cmd := exec.CommandContext(ctx, LookPath(req.Argv[0], env), req.Argv[1:]...)
	cmd.Dir, cmd.Env = s.dir, env
	if req.Dir != "" {
		cmd.Dir = req.Dir
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
		p.copyOutput(ctx, output, frameStdout, frameStderr)
		return p, nil
	}
	tty, err := startTTY(cmd, req.Size)
	if err != nil {
		return nil, err
	}
	p.tty, p.stdin, p.outputs = tty, tty, []*os.File{tty}
	p.copyOutput(ctx, output, frameStdout)
	return p, nil
}

// startPipes starts cmd in a process group of its own, with pipes for its
// standard streams.
//
// The output pipes are the server's, not os/exec's: Wait would wait for
// os/exec's to be drained, and a background process the command left behind
// holds them open, so every such command would take OutputDelay to end.
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
func (p *process) copyOutput(ctx context.Context, output func(kind byte) *frameWriter, kinds ...byte) {
	if p.trace != nil {
		p.trace.InfoContext(ctx, "claude started",
			slog.Int("pid", p.cmd.Process.Pid),
			slog.Float64("spawn_ms", float64(time.Since(p.start))/float64(time.Millisecond)))
	}
	for i, out := range p.outputs {
		p.output.Go(func() {
			w := output(kinds[i])
			if p.trace != nil && kinds[i] == frameStdout {
				first, send := true, w.send
				w.send = func(frame []byte) error {
					if first {
						p.trace.InfoContext(ctx, "claude first stdout",
							slog.Int("pid", p.cmd.Process.Pid),
							slog.Float64("since_request_ms", float64(time.Since(p.start))/float64(time.Millisecond)))
						first = false
					}
					return send(frame)
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
	// hold an output open; it gets OutputDelay. Closing the terminal also ends
	// a write of stdin the command never read (Wait closes the stdin pipe).
	deadline := time.Now().Add(OutputDelay)
	for _, out := range p.outputs {
		_ = out.SetReadDeadline(deadline)
	}
	p.output.Wait()
	p.closeOutputs()
	return ExitCode(err, log)
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
		default:
			p.control(data)
		}
	}
}

// control applies a frame of a terminal size or a signal.
func (p *process) control(data []byte) {
	switch data[0] {
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

// queue puts data on stdin, unless ctx ends first.
func queue(ctx context.Context, stdin chan<- []byte, data []byte) bool {
	select {
	case stdin <- data:
		return true
	case <-ctx.Done():
		return false
	}
}

// writeStdin writes what is queued to the command's stdin, and closes it at
// nil. After that, or a failed write (the command closed its stdin or
// exited), it drops the rest, so that whoever queues is never held up. It
// ends when stdin is closed or over is.
func writeStdin(p *process, stdin <-chan []byte, over <-chan struct{}) {
	done := false
	for {
		var data []byte
		var ok bool
		select {
		case data, ok = <-stdin:
			if !ok {
				return
			}
		case <-over:
			return
		}
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

// ExitCode returns the exit code of a command whose Wait returned err: its
// own, 128 plus the signal that ended it, or the code of one that couldn't
// start, logged, if it can't be told.
func ExitCode(err error, log *slog.Logger) int {
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

// LookPath returns the path of program name on the last PATH in env, or
// name as it is if it names a path or isn't found there, for exec to find
// or fail on.
func LookPath(name string, env []string) string {
	if strings.Contains(name, "/") {
		return name
	}
	path := ""
	for _, v := range env {
		if p, ok := strings.CutPrefix(v, "PATH="); ok {
			path = p
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		if p, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return p
		}
	}
	return name
}
