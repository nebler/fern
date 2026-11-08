package boxexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ErrSessionGone means the box doesn't have the session any more: it stopped,
// or the command waited too long for a connection.
var ErrSessionGone = errors.New("the command is gone: its box stopped, or it had no connection for too long")

// Resume says how RunSession gets through a connection that breaks.
type Resume struct {
	// Dial opens a new connection to the command's box.
	Dial func(ctx context.Context) (*websocket.Conn, error)
	// For is how long it keeps trying; 0 means it doesn't.
	For time.Duration
	// Abandon reports whether the caller is going away with the command left
	// for a later one to find, whatever happens to ctx. May be nil.
	Abandon func() bool
}

// RunSession starts req on conn as a session (session.go), connects it to s,
// and returns the command's exit code, as Run does; req.Session must be set.
// A connection that breaks is replaced through resume, and the command, its
// output and its stdin carry on with nothing lost or repeated. It closes the
// connections it uses.
func RunSession(ctx context.Context, conn *websocket.Conn, req Request, s Streams, resume Resume) (int, error) {
	c := &sessionClient{req: req, std: s, resume: resume, done: make(chan struct{})}
	c.changed.L = &c.mu
	defer func() {
		close(c.done)
		c.wake()
	}()
	if s.Stdin != nil {
		go c.readStdin(s.Stdin)
	} else {
		c.in = [][]byte{{frameStdinEOF}}
	}
	for {
		code, broke, err := c.attach(ctx, conn)
		if !broke {
			return code, err
		}
		if conn, err = c.redial(ctx, err); err != nil {
			return 0, err
		}
	}
}

// sessionClient is the client's side of a session.
type sessionClient struct {
	req    Request
	std    Streams
	resume Resume
	done   chan struct{} // closed when RunSession returns

	// out is how many output frames have come. Only attach touches it, and
	// one runs at a time.
	out      uint64
	unacked  int
	attached bool // a connection has carried the request

	mu      sync.Mutex
	changed sync.Cond // on mu: in, or the connection or ctx ended
	in      [][]byte  // stdin frames from number inBase on, unacknowledged
	inBase  uint64
	inSize  int
}

func (c *sessionClient) wake() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.changed.Broadcast()
}

// readStdin takes in stdin until it ends, waiting while too much is
// unacknowledged. A Read that blocks when the command exits can't be
// interrupted: it ends at its next one.
func (c *sessionClient) readStdin(stdin io.Reader) {
	buf := make([]byte, frameData)
	for {
		n, err := stdin.Read(buf)
		if n > 0 && !c.keep(append([]byte{frameStdin}, buf[:n]...)) {
			return
		}
		if err != nil {
			c.keep([]byte{frameStdinEOF})
			return
		}
	}
}

// keep adds a stdin frame, and reports whether anyone still wants one.
func (c *sessionClient) keep(frame []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.inSize >= sessionWindow && !c.over() {
		c.changed.Wait()
	}
	if c.over() {
		return false
	}
	c.in = append(c.in, frame)
	c.inSize += len(frame)
	c.changed.Broadcast()
	return true
}

func (c *sessionClient) over() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// ackIn forgets the stdin frames before number n.
func (c *sessionClient) ackIn(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.inBase < n && len(c.in) > 0 {
		c.inSize -= len(c.in[0])
		c.in[0] = nil
		c.in = c.in[1:]
		c.inBase++
	}
	c.changed.Broadcast()
}

// sendStdin sends the stdin frames from number next on, until ctx ends.
func (c *sessionClient) sendStdin(ctx context.Context, conn *websocket.Conn, next uint64) {
	for {
		c.mu.Lock()
		for next >= c.inBase+uint64(len(c.in)) && ctx.Err() == nil {
			c.changed.Wait()
		}
		if ctx.Err() != nil {
			c.mu.Unlock()
			return
		}
		next = max(next, c.inBase)
		frame := c.in[next-c.inBase]
		c.mu.Unlock()
		if conn.Write(ctx, websocket.MessageBinary, frame) != nil {
			return
		}
		next++
	}
}

// attach runs the session over conn until the command ends or conn does. It
// returns broke when conn just broke, so that another may carry on.
func (c *sessionClient) attach(ctx context.Context, conn *websocket.Conn) (code int, broke bool, err error) {
	// The connection's own calls don't watch ctx: they'd close it with no
	// close message, which to the server means the daemon went away.
	stopWatch := context.AfterFunc(ctx, func() { c.closeConn(conn, false) })
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	wake := context.AfterFunc(cctx, c.wake)
	var senders sync.WaitGroup
	defer func() {
		stopWatch()
		c.closeConn(conn, broke)
		cancel()
		wake()
		c.wake()
		senders.Wait()
	}()
	// Only the first connection may start the command: a later one that finds
	// it gone must not run it again.
	req := c.req
	req.Seen = c.out
	resuming := c.attached
	c.attached = true
	c.req = Request{Session: c.req.Session}
	if err := wsjson.Write(cctx, conn, req); err != nil {
		return 0, ctx.Err() == nil, fmt.Errorf("send request: %w", err)
	}
	if c.std.Resize != nil || c.std.Signal != nil {
		senders.Go(func() { sendControls(cctx, conn, c.std.Resize, c.std.Signal) })
	}
	// On a later connection, stdin waits to hear where the server got to. A
	// box still running an older fern-init never says, and has seen none yet.
	sending := !resuming
	if !resuming {
		senders.Go(func() { c.sendStdin(cctx, conn, 0) })
	}
	r := &messageReader{conn: conn}
	for {
		typ, data, err := r.read(cctx)
		if err != nil {
			// A close message is the daemon saying the command has ended.
			return 0, ctx.Err() == nil && websocket.CloseStatus(err) == -1, err
		}
		if typ == websocket.MessageText {
			var e exit
			if err := json.Unmarshal(data, &e); err != nil {
				return 0, false, fmt.Errorf("bad exit message: %w", err)
			}
			return e.Code, false, nil
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case frameAck:
			if len(data) != 9 {
				continue
			}
			n := binary.BigEndian.Uint64(data[1:])
			c.ackIn(n)
			if !sending {
				sending = true
				senders.Go(func() { c.sendStdin(cctx, conn, n) })
			}
		case frameLost:
			if len(data) == 9 {
				_, _ = fmt.Fprint(c.std.Stderr, "[… output lost while Fern restarted …]\n")
			}
		case frameGone:
			return 0, false, ErrSessionGone
		case frameStdout, frameStderr:
			w := c.std.Stdout
			if data[0] == frameStderr {
				w = c.std.Stderr
			}
			if _, err := w.Write(data[1:]); err != nil {
				return 0, false, fmt.Errorf("write output: %w", err)
			}
			c.out++
			c.unacked += len(data)
			if c.unacked >= sessionAckBytes {
				c.unacked = 0
				if err := conn.Write(cctx, websocket.MessageBinary, countFrame(frameAck, c.out)); err != nil {
					return 0, ctx.Err() == nil, err
				}
			}
		}
	}
}

// closeConn ends a connection attach has finished with. A close message tells
// the server the client is done, which kills a command still running; so it
// isn't sent on one that broke, or when the command is meant to run on.
func (c *sessionClient) closeConn(conn *websocket.Conn, broke bool) {
	if broke || (c.resume.Abandon != nil && c.resume.Abandon()) {
		_ = conn.CloseNow()
		return
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// redial opens a new connection to the session, until resume.For has passed
// since the last one broke with cause.
func (c *sessionClient) redial(ctx context.Context, cause error) (*websocket.Conn, error) {
	if c.resume.For <= 0 || c.resume.Dial == nil {
		return nil, cause
	}
	ctx, cancel := context.WithTimeout(ctx, c.resume.For)
	defer cancel()
	wait := 50 * time.Millisecond
	for {
		conn, err := c.resume.Dial(ctx)
		if err == nil {
			return conn, nil
		}
		var refused *DialError
		if errors.As(err, &refused) && refused.StatusCode < http.StatusInternalServerError {
			return nil, err // no such environment: nothing to wait for
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the connection broke and didn't come back: %w", cause)
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}
