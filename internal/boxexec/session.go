package boxexec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// A session is a command that can outlive its connection: a Request with a
// Session. Both sides number the frames they send from 0, keep them until
// the other side acknowledges them, and start again where the other side
// got to on a new connection, so a dropped connection loses and repeats
// nothing. This is what carries a running command across a restart of the
// daemon, which relays every connection and is how Fern upgrades.
//
// A connection that ends with a close message means the client is done: the
// command is killed, as an exec's is. One that just breaks means the daemon
// went away: the command runs on for Server.Grace, waiting for a connection
// to bring it back.

// DefaultGrace is how long a session's command runs with no connection.
const DefaultGrace = 2 * time.Minute

const (
	// sessionWindow is how many bytes of unacknowledged frames each side
	// keeps. A command printing more than this waits for its client, as it
	// would on a slow connection.
	sessionWindow = 1 << 20
	// sessionAckBytes is how many bytes of output a client gets between
	// acknowledgements.
	sessionAckBytes = sessionWindow / 4
)

// session is a command and what it has printed that its client hasn't
// acknowledged. Its first request's handler owns it (Server.runSession); the
// handlers of later connections only serve them.
type session struct {
	grace time.Duration
	kill  context.CancelFunc // kills the command
	stdin chan []byte
	over  chan struct{} // closed when the command's handler is done

	killed    chan struct{} // closed by end: nobody will read the output
	delivered chan struct{} // closed once the client has the exit code

	mu        sync.Mutex
	changed   sync.Cond // on mu: frames, ended, killed, or the connection changed
	frames    [][]byte  // output frames from number base on
	base      uint64
	size      int  // bytes in frames
	ended     bool // the command has exited with code
	code      int
	stdinSeen uint64   // stdin frames, the end included, taken from clients
	p         *process // nil until the command has started
	conn      *websocket.Conn
	timer     *time.Timer // set while there is no connection
	gone      bool        // killed is closed
	got       bool        // delivered is closed
}

func newSession(grace time.Duration, kill context.CancelFunc) *session {
	s := &session{
		grace: grace, kill: kill,
		stdin: make(chan []byte, stdinQueue), over: make(chan struct{}),
		killed: make(chan struct{}), delivered: make(chan struct{}),
	}
	s.changed.L = &s.mu
	return s
}

// add keeps an output frame until it is acknowledged, waiting while too much
// is. It fails once nobody will read it, which ends the copy of the output.
func (s *session) add(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.size >= sessionWindow && !s.gone {
		s.changed.Wait()
	}
	if s.gone {
		return errNoReader
	}
	s.frames = append(s.frames, bytes.Clone(frame))
	s.size += len(frame)
	s.changed.Broadcast()
	return nil
}

// abandon kills the command and lets everything waiting for its client go.
func (s *session) abandon() {
	s.kill()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gone {
		s.gone = true
		close(s.killed)
		s.changed.Broadcast()
	}
	if s.timer != nil {
		s.timer.Stop()
	}
}

// end records the command's exit.
func (s *session) end(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended, s.code = true, code
	s.changed.Broadcast()
}

// ack forgets the frames before number n.
func (s *session) ack(n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.base < n && len(s.frames) > 0 {
		s.size -= len(s.frames[0])
		s.frames[0] = nil
		s.frames = s.frames[1:]
		s.base++
	}
	s.changed.Broadcast()
}

// serve serves one connection until it ends. A later connection takes over
// from this one.
func (s *session) serve(ctx context.Context, conn *websocket.Conn, seen uint64) {
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.CloseNow()
	}
	s.conn = conn
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	stdinSeen := s.stdinSeen
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.changed.Broadcast()
	})
	defer stop()
	// The client resends stdin from here, and knows the session is there.
	if conn.Write(ctx, websocket.MessageBinary, countFrame(frameAck, stdinSeen)) != nil {
		s.dropped(conn, false)
		return
	}
	var sender sync.WaitGroup
	sender.Go(func() { s.send(ctx, conn, seen) })
	err := s.read(ctx, conn)
	cancel()
	sender.Wait()
	s.dropped(conn, websocket.CloseStatus(err) != -1)
}

// dropped handles the end of connection conn: closed says the client ended
// it with a close message, not by going away.
func (s *session) dropped(conn *websocket.Conn, closed bool) {
	s.mu.Lock()
	if s.conn != conn {
		s.mu.Unlock()
		return // a later connection has taken over
	}
	s.conn = nil
	done := false
	switch {
	case closed && s.ended:
		if !s.got {
			s.got = true
			close(s.delivered)
		}
	case closed:
		done = true // the client has finished with the command
	case !s.gone:
		s.timer = time.AfterFunc(s.grace, s.abandon)
	}
	s.mu.Unlock()
	if done {
		s.abandon()
	}
}

// send sends the frames from number next on, then the exit code.
func (s *session) send(ctx context.Context, conn *websocket.Conn, next uint64) {
	for {
		s.mu.Lock()
		if next < s.base {
			// The client lost what it had: a new daemon, for a command it ran.
			lost := s.base - next
			next = s.base
			s.mu.Unlock()
			if conn.Write(ctx, websocket.MessageBinary, countFrame(frameLost, lost)) != nil {
				return
			}
			continue
		}
		for next >= s.base+uint64(len(s.frames)) && !s.ended && ctx.Err() == nil {
			s.changed.Wait()
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		if next < s.base+uint64(len(s.frames)) {
			frame := s.frames[next-s.base]
			s.mu.Unlock()
			if conn.Write(ctx, websocket.MessageBinary, frame) != nil {
				return
			}
			next++
			continue
		}
		code := s.code
		s.mu.Unlock()
		// The client's close message, which read waits for, says it got this.
		_ = wsjson.Write(ctx, conn, exit{Code: code})
		return
	}
}

// read applies what the client sends until the connection ends, and returns
// why it did.
func (s *session) read(ctx context.Context, conn *websocket.Conn) error {
	r := &messageReader{conn: conn}
	for {
		_, data, err := r.read(ctx)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case frameStdin, frameStdinEOF:
			var chunk []byte
			if data[0] == frameStdin {
				chunk = bytes.Clone(data[1:])
			}
			if !s.queue(ctx, chunk) {
				return ctx.Err()
			}
			s.mu.Lock()
			s.stdinSeen++
			n := s.stdinSeen
			s.mu.Unlock()
			if err := conn.Write(ctx, websocket.MessageBinary, countFrame(frameAck, n)); err != nil {
				return err
			}
		case frameAck:
			if len(data) == 9 {
				s.ack(binary.BigEndian.Uint64(data[1:]))
			}
		default:
			s.mu.Lock()
			p := s.p
			s.mu.Unlock()
			if p != nil {
				p.control(data)
			}
		}
	}
}

// queue puts data on stdin, unless ctx ends or the command has first. A
// terminal has no EOF of its own: its end is dropped.
func (s *session) queue(ctx context.Context, data []byte) bool {
	s.mu.Lock()
	tty := s.p != nil && s.p.tty != nil
	s.mu.Unlock()
	if data == nil && tty {
		return true
	}
	select {
	case s.stdin <- data:
		return true
	case <-s.over:
		return true
	case <-ctx.Done():
		return false
	}
}

// countFrame is a frame carrying one number.
func countFrame(kind byte, n uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{kind}, n)
}

// errNoReader is what adding a frame returns once nobody will read it.
var errNoReader = errors.New("session gone")

// runSession starts req's command as session id, serves conn, and returns
// once the command has ended and its client knows, or never will.
func (s *Server) runSession(ctx context.Context, conn *websocket.Conn, id string, req Request) {
	s.running(1)
	defer s.running(-1)
	cmdCtx, kill := context.WithCancel(ctx)
	defer kill()
	sess := newSession(s.Grace, kill)
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions, s.finished = make(map[string]*session), make(map[string]int)
	}
	s.sessions[id] = sess
	s.mu.Unlock()
	code, delivered := s.superviseSession(ctx, cmdCtx, conn, sess, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	if !delivered {
		return
	}
	// A retry of a call whose answer was lost learns how it ended, and doesn't
	// run the command again.
	s.finished[id] = code
	time.AfterFunc(s.Grace, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.finished, id)
	})
}

// superviseSession runs sess's command to its end. It returns the exit code,
// and whether the client got it.
func (s *Server) superviseSession(ctx, cmdCtx context.Context, conn *websocket.Conn, sess *session, req Request) (int, bool) {
	// The box stopping lets go of whoever waits for the client.
	stop := context.AfterFunc(ctx, sess.abandon)
	defer stop()
	defer sess.abandon()
	var streams sync.WaitGroup
	defer streams.Wait()
	// Nothing reads a frame's error but the copy of the output.
	p, err := s.start(cmdCtx, req, func(kind byte) *frameWriter {
		w := &frameWriter{send: sess.add, buf: make([]byte, 1+initialFrameData)}
		w.buf[0] = kind
		return w
	})
	code := exitNotStarted
	if err != nil {
		_ = sess.add(append([]byte{frameStderr}, []byte("exec: "+err.Error()+"\n")...))
		close(sess.over)
	} else {
		sess.mu.Lock()
		sess.p = p
		sess.mu.Unlock()
		streams.Go(func() { writeStdin(p, sess.stdin, sess.over) })
	}
	streams.Go(func() { sess.serve(ctx, conn, req.Seen) })
	if err == nil {
		code = p.wait(s.log)
		close(sess.over)
	}
	sess.end(code)
	return code, sess.waitDelivered()
}

// waitDelivered waits until the client has the exit code, or never will.
func (s *session) waitDelivered() bool {
	select {
	case <-s.delivered:
		return true
	case <-s.killed:
		return false
	}
}

// serveSession serves a Request with a Session: it takes up the session, or
// starts it if it is a new one.
func (s *Server) serveSession(ctx context.Context, conn *websocket.Conn, req Request) {
	s.mu.Lock()
	sess := s.sessions[req.Session]
	code, finished := s.finished[req.Session]
	s.mu.Unlock()
	switch {
	case sess != nil:
		sess.serve(ctx, conn, req.Seen)
	case finished:
		// All that is left of it: its client asked again because it lost it.
		_ = conn.Write(ctx, websocket.MessageBinary, countFrame(frameLost, 0))
		finish(ctx, conn, code)
	case len(req.Argv) == 0:
		_ = conn.Write(ctx, websocket.MessageBinary, []byte{frameGone})
		_ = conn.Close(websocket.StatusNormalClosure, "")
	default:
		s.runSession(ctx, conn, req.Session, req)
	}
}
