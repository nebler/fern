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

// outcome is how a session ends for its client. It is open until settle
// ends it, once: what comes first stays.
type outcome int

const (
	// open: the client may still come for the output and the exit code.
	open outcome = iota
	// delivered: the client has the exit code, and said so by closing.
	delivered
	// abandoned: nobody will read the output. The client closed before the
	// command ended, no connection came within the grace, or the box stops.
	abandoned
)

// link is one connection serving a session. A session has at most one, and
// the next takes over only once the one before has let go (serve).
type link struct {
	conn   *websocket.Conn
	cancel context.CancelFunc // ends its serve
	done   chan struct{}      // closed when its serve has returned
}

// session is a command, what it has printed that its client hasn't
// acknowledged, and the stdin it hasn't taken. Its first request's handler
// owns it (Server.runSession); the handlers of later connections only serve
// them.
type session struct {
	grace   time.Duration
	kill    context.CancelFunc // kills the command
	stdin   chan []byte        // to the command, from feed
	over    chan struct{}      // closed when the command's handler is done
	settled chan struct{}      // closed when outcome leaves open
	// opened is closed once the first connection is the session's: a later
	// one waits for it (serveSession). The session is listed before its
	// command starts, and its first connection served only after that; a
	// second connection that came in between was served first, and the
	// first then took over from it and gave the command the stdin it had
	// buffered a second time (a test saw it once in 150 runs, 2026-11-13).
	opened chan struct{}

	mu      sync.Mutex
	changed sync.Cond // on mu: any of what follows changed, or a connection's context ended
	outcome outcome
	link    *link       // the connection the client is on; nil while it has none
	timer   *time.Timer // runs while it has none and is awaited: abandons the session at the grace's end
	frames  [][]byte    // output frames from number base on
	base    uint64
	size    int  // bytes in frames
	ended   bool // the command has exited with code
	code    int
	in      [][]byte // stdin frames the command hasn't taken; a nil one is stdin's end
	inGot   uint64   // stdin frames that came from clients, the end included: where the next connection goes on
	inTaken uint64   // those the command has taken: what the client may forget
	p       *process // nil until the command has started
}

func newSession(grace time.Duration, kill context.CancelFunc) *session {
	s := &session{
		grace: grace, kill: kill,
		stdin: make(chan []byte), over: make(chan struct{}), settled: make(chan struct{}),
		opened: make(chan struct{}),
	}
	s.changed.L = &s.mu
	return s
}

// settle ends the session for its client as o, unless it has ended. The
// caller holds mu.
func (s *session) settle(o outcome) {
	if s.outcome != open {
		return
	}
	s.outcome = o
	close(s.settled)
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.changed.Broadcast()
}

// add keeps an output frame until it is acknowledged, waiting while too much
// is. It fails once nobody will read it, which ends the copy of the output.
func (s *session) add(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.size >= sessionWindow && s.outcome == open {
		s.changed.Wait()
	}
	if s.outcome != open {
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
	s.settle(abandoned)
}

// end records the command's exit.
func (s *session) end(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended, s.code = true, code
	s.in = nil // nothing takes it any more
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

// take keeps a stdin frame from a client, or stdin's end (nil), for the
// command. It never waits: the connection's reader must go on reading, as
// the client's acknowledgements of the output come on the same connection,
// and a command that waits for those before it prints more is one that takes
// no stdin meanwhile. What bounds the frames kept is the client, which sends
// no more than sessionWindow bytes past the last frame it was told to forget.
func (s *session) take(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inGot++
	if s.ended {
		return
	}
	s.in = append(s.in, data)
	s.changed.Broadcast()
}

// feed gives the command its stdin, frame by frame, until the command has
// ended. A frame counts as taken, and its client is told to forget it, once
// the command's stdin has it.
func (s *session) feed() {
	for {
		s.mu.Lock()
		for len(s.in) == 0 && !s.ended {
			s.changed.Wait()
		}
		if s.ended {
			s.mu.Unlock()
			return
		}
		frame := s.in[0]
		tty := s.p != nil && s.p.tty != nil
		s.mu.Unlock()
		// A terminal has no EOF of its own: its end is dropped.
		if frame != nil || !tty {
			select {
			case s.stdin <- frame:
			case <-s.over:
				return
			}
		}
		s.mu.Lock()
		if len(s.in) > 0 { // end has not cleared it
			s.in[0] = nil
			s.in = s.in[1:]
		}
		s.inTaken++
		s.changed.Broadcast()
		s.mu.Unlock()
	}
}

// serve serves one connection until it ends. A later connection takes over
// from this one.
func (s *session) serve(ctx context.Context, conn *websocket.Conn, seen uint64) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l := &link{conn: conn, cancel: cancel, done: make(chan struct{})}
	defer close(l.done)
	s.mu.Lock()
	old := s.link
	s.link = l
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	select {
	case <-s.opened:
	default:
		close(s.opened)
	}
	s.mu.Unlock()
	if old != nil {
		// The connection before this one may have read a stdin frame it has
		// yet to count. Were the client told where to go on before that, it
		// would send the frame a second time.
		old.cancel()
		_ = old.conn.CloseNow()
		<-old.done
	}
	s.mu.Lock()
	got := s.inGot
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.changed.Broadcast()
	})
	defer stop()
	// The client resends stdin from here, and knows the session is there.
	if conn.Write(ctx, websocket.MessageBinary, countFrame(frameAck, got)) != nil {
		s.dropped(l, false)
		return
	}
	var sender sync.WaitGroup
	sender.Go(func() { s.send(ctx, conn, seen, got) })
	err := s.read(ctx, conn)
	cancel()
	sender.Wait()
	s.dropped(l, websocket.CloseStatus(err) != -1)
}

// dropped handles the end of l's connection: closed says the client ended
// it with a close message, not by going away.
func (s *session) dropped(l *link, closed bool) {
	s.mu.Lock()
	if s.link != l {
		s.mu.Unlock()
		return // a later connection has taken over
	}
	s.link = nil
	done := false
	switch {
	case s.outcome != open:
	case closed && s.ended:
		s.settle(delivered)
	case closed:
		done = true // the client has finished with the command
	default:
		s.timer = time.AfterFunc(s.grace, s.abandon)
	}
	s.mu.Unlock()
	if done {
		s.abandon()
	}
}

// send sends the output frames from number next on, then the exit code, and
// meanwhile tells the client of the stdin frames the command has taken past
// number acked.
func (s *session) send(ctx context.Context, conn *websocket.Conn, next, acked uint64) {
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
		for next >= s.base+uint64(len(s.frames)) && !s.ended && s.inTaken <= acked && ctx.Err() == nil {
			s.changed.Wait()
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		if taken := s.inTaken; taken > acked {
			s.mu.Unlock()
			if conn.Write(ctx, websocket.MessageBinary, countFrame(frameAck, taken)) != nil {
				return
			}
			acked = taken
			continue
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
		case frameStdin:
			s.take(bytes.Clone(data[1:]))
		case frameStdinEOF:
			s.take(nil)
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
		streams.Go(sess.feed)
	}
	streams.Go(func() { sess.serve(ctx, conn, req.Seen) })
	if err == nil {
		code = p.wait(s.log)
		close(sess.over)
	}
	sess.end(code)
	return code, sess.waitSettled()
}

// waitSettled waits until the client has the exit code, or never will, and
// says which.
func (s *session) waitSettled() bool {
	<-s.settled
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome == delivered
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
		// Not before the connection that started it: it would take over
		// from this one.
		select {
		case <-sess.opened:
		case <-sess.settled:
		case <-ctx.Done():
			return
		}
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
