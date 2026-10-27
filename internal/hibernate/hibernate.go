// Package hibernate runs Claude in a box so that the box can sleep between
// turns while T3 keeps Claude's session open.
//
// T3 keeps a thread's Claude running between turns and ends it only after
// minutes without one; meanwhile its box stays awake, holding Claude's and
// the services' memory and CPU. Run stands in for that Claude: once Claude
// has had nothing to do for a while (its turn finished, no answer pending
// either way, no background task), it stops Claude, which saves its
// session, and the box sleeps. The SDK's next line starts Claude again with
// --resume, after the setup the SDK sent the first one (initialize, MCP
// servers, model, modes), whose answers the SDK doesn't see; then the line
// goes in. To T3 it is the same process all along, a slower one at the
// first line after a stop.
package hibernate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/boxexec"
)

// Exec runs a command in the environment's box, waking it if needed, until
// the command exits, and lets the box sleep as soon as it has: the API
// client's Exec.
type Exec func(ctx context.Context, req boxexec.Request, s boxexec.Streams) (int, error)

// errStopped ends a stopped Claude's stdin.
var errStopped = errors.New("claude stopped")

// stopTimeout is how long a Claude sent SIGTERM may take to exit before its
// exec is cut off; the SDK's lines wait for the next one meanwhile.
const stopTimeout = 10 * time.Second

// Run runs req with exec, connected to s, and returns its exit code. A
// Claude driven by its SDK (stream-json in and out) is stopped after idle
// with nothing to do and started again at the SDK's next line; any other
// command, or an idle of 0, just runs.
//
// s.Stdin is read in a goroutine that ends at its next Read after Run
// returns, if it is blocked then.
func Run(ctx context.Context, exec Exec, req boxexec.Request, s boxexec.Streams, idle time.Duration) (int, error) {
	if idle <= 0 || s.Stdin == nil || !Applies(req) {
		return exec(ctx, req, s)
	}
	h := &hibernator{exec: exec, req: req, std: s, idle: idle, st: newStream()}
	return h.run(ctx)
}

// Applies says whether req runs a Claude that Run can stop and resume
// unseen: one its SDK drives, with stream-json in and out, without a
// terminal.
func Applies(req boxexec.Request) bool { return !req.TTY && isStreamingClaude(req.Argv) }

// hibernator is one Run: Claude, and the processes standing in for it one
// after another.
type hibernator struct {
	exec Exec
	req  boxexec.Request
	std  boxexec.Streams
	idle time.Duration
	st   *stream
}

// claude is one Claude process: its stdin, the signals sent to it and its
// end.
type claude struct {
	stdin  *io.PipeWriter
	sigs   chan os.Signal
	cancel context.CancelFunc // cuts its exec off
	done   chan result        // buffered
}

type result struct {
	code int
	err  error
}

func (h *hibernator) run(ctx context.Context) (int, error) {
	done := make(chan struct{})
	defer close(done)
	lines := readLines(done, h.std.Stdin)
	c := h.start(ctx, h.req)
	var (
		waiting  [][]byte  // lines for the next Claude, read while one stopped
		eof      bool      // the SDK's stdin has ended
		stopping time.Time // when c was sent SIGTERM because it was idle; zero if it wasn't
		ending   bool      // T3 is ending Claude: c's exit is Run's
		code     int       // the exit code of the Claude stopped last
	)
	tick := time.NewTicker(min(max(h.idle/10, 10*time.Millisecond), time.Second))
	defer tick.Stop()
	for {
		var exited chan result
		if c != nil {
			exited = c.done
		}
		select {
		case line, ok := <-lines:
			if !ok {
				lines, eof = nil, true
				switch {
				case c == nil:
					// Stopped, and nothing more to say: as Claude would on EOF.
					return 0, nil
				case stopping.IsZero():
					ending = true
					_ = c.stdin.Close()
				}
				continue
			}
			if !stopping.IsZero() {
				waiting = append(waiting, line)
				continue
			}
			if c == nil {
				c = h.restart(ctx)
			}
			h.send(c, line)
		case sig := <-h.std.Signal:
			if c == nil {
				// T3 ending a stopped Claude: it ended as it does on a signal.
				return code, nil
			}
			ending = true
			c.sigs <- sig
		case r := <-exited:
			if stopping.IsZero() || ending {
				return r.code, r.err
			}
			// Stopped; cut off if it took too long, with the code of a SIGTERM.
			c, stopping, code = nil, time.Time{}, r.code
			if r.err != nil {
				code = 128 + int(syscall.SIGTERM)
			}
			if len(waiting) > 0 {
				c = h.restart(ctx)
				for _, line := range waiting {
					h.send(c, line)
				}
				waiting = nil
			}
			if eof {
				if c == nil {
					return 0, nil
				}
				ending = true
				_ = c.stdin.Close()
			}
		case <-tick.C:
			switch {
			case c == nil || ending:
			case !stopping.IsZero():
				if time.Since(stopping) >= stopTimeout {
					c.cancel()
				}
			default:
				if since, ok := h.st.idleSince(); ok && time.Since(since) >= h.idle {
					stopping = time.Now()
					c.sigs <- syscall.SIGTERM
				}
			}
		}
	}
}

// start runs a Claude with req.
func (h *hibernator) start(ctx context.Context, req boxexec.Request) *claude {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	c := &claude{stdin: pw, sigs: make(chan os.Signal, 4), cancel: cancel, done: make(chan result, 1)}
	out := &lineWriter{w: h.std.Stdout, keep: h.st.Out}
	go func() {
		defer cancel()
		code, err := h.exec(ctx, req, boxexec.Streams{Stdin: pr, Stdout: out, Stderr: h.std.Stderr, Signal: c.sigs})
		if ferr := out.flush(); err == nil {
			err = ferr
		}
		// Ends the read of stdin that exec left waiting, and any write to it.
		pr.CloseWithError(errStopped)
		c.done <- result{code, err}
	}()
	return c
}

// restart starts Claude again in its session and sends it the setup.
func (h *hibernator) restart(ctx context.Context) *claude {
	argv, setup := h.st.restart(h.req.Argv)
	req := h.req
	req.Argv = argv
	c := h.start(ctx, req)
	for _, line := range setup {
		// A failed write means Claude has ended; its result says why.
		_, _ = c.stdin.Write(line)
	}
	return c
}

// send gives line to c.
func (h *hibernator) send(c *claude, line []byte) {
	h.st.In(line)
	_, _ = c.stdin.Write(line)
}

// readLines sends r's lines, each with its newline (the last may have
// none), until r ends or done is closed, then closes the channel.
func readLines(done <-chan struct{}, r io.Reader) <-chan []byte {
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				select {
				case lines <- line:
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return lines
}

// lineWriter passes on, whole, the lines written to it that keep allows.
type lineWriter struct {
	w    io.Writer
	keep func(line []byte) bool
	buf  []byte // the start of a line, not yet ended
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	rest := l.buf
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		if l.keep(rest[:i]) {
			if _, err := l.w.Write(rest[:i+1]); err != nil {
				return 0, err
			}
		}
		rest = rest[i+1:]
	}
	l.buf = append(l.buf[:0], rest...)
	return len(p), nil
}

// flush passes on what is left of an unended line.
func (l *lineWriter) flush() error {
	if len(l.buf) == 0 || !l.keep(l.buf) {
		return nil
	}
	_, err := l.w.Write(l.buf)
	l.buf = l.buf[:0]
	return err
}
