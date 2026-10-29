// Package boxexec is the exec protocol spoken by the CLI, the daemon and
// fern-init: one WebSocket per command.
//
// The client sends a JSON Request as a text message, then binary frames for
// stdin, stdin EOF, terminal resizes and signals. The server sends binary
// frames for stdout and stderr, then the exit code as a JSON text message,
// and closes. Every binary frame starts with one byte saying what it carries.
//
// The server side is in server.go, the client side in client.go, and the
// daemon's relay between the two in proxy.go.
package boxexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"github.com/coder/websocket"
)

// Port is the port fern-init serves exec on inside every box.
const Port = 7070

// Path is the URL path of fern-init's exec endpoint.
const Path = "/exec"

// ReadyPath is the URL path where fern-init says whether the box's services
// are ready; with ?wait=DURATION it answers as soon as they are.
const ReadyPath = "/readyz"

// StopPath is the authenticated endpoint that starts fern-init's graceful shutdown.
const StopPath = "/stop"

// Frame kinds: the first byte of every binary message.
const (
	frameStdin byte = iota
	frameStdout
	frameStderr
	frameStdinEOF
	frameResize // four more bytes: rows, then columns, as big-endian uint16
	frameSignal // one more byte: the signal number
)

// maxMessage is the largest message a connection reads, more than the
// WebSocket library allows by default (32 KiB): frames carry at most
// 1+frameData bytes, and the JSON Request (argv and environment) the rest.
const maxMessage = 1 << 20

// Request starts a command. It is the first message on a connection.
type Request struct {
	Argv []string `json:"argv"`
	TTY  bool     `json:"tty,omitempty"`
	Size Size     `json:"size,omitzero"` // initial terminal size when TTY is set
	// Dir is where the command runs, an absolute path; the workspace if
	// empty.
	Dir string `json:"dir,omitempty"`
	// Environ adds NAME=VALUE variables to the command's, replacing any of
	// the same name.
	Environ []string `json:"environ,omitempty"`
}

// Streams connect a command to the client: its standard streams, terminal
// sizes and signals.
type Streams struct {
	Stdin          io.Reader // nil: the command reads EOF at once
	Stdout, Stderr io.Writer
	Resize         <-chan Size // new terminal sizes; may be nil
	// Signal sends signals to the command's process group; the server passes
	// on only SIGHUP, SIGINT and SIGTERM. May be nil.
	Signal <-chan os.Signal
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

// frameData is the most data the sides put in one frame they send.
const frameData = 64 << 10

// initialFrameData keeps quiet execs small; a busy stream grows to frameData.
const initialFrameData = 4 << 10

// frameWriter sends what is written to it as frames of one kind, through one
// buffer it reuses. It is not safe for concurrent use.
type frameWriter struct {
	send func([]byte) error // sends one frame; conn.Write doesn't keep it
	buf  []byte             // the frame: buf[0] is the kind, then the data
}

// frames returns a writer that sends what is written to it as frames of one
// kind.
func frames(ctx context.Context, conn *websocket.Conn, kind byte) *frameWriter {
	w := &frameWriter{
		send: func(frame []byte) error { return conn.Write(ctx, websocket.MessageBinary, frame) },
		buf:  make([]byte, 1+initialFrameData),
	}
	w.buf[0] = kind
	return w
}

// Write sends p as one frame, or as several if it is longer than frameData.
func (w *frameWriter) Write(p []byte) (int, error) {
	if size := 1 + min(len(p), frameData); size > len(w.buf) {
		w.grow(size)
	}
	var n int
	for len(p) > 0 {
		c := copy(w.buf[1:], p)
		if err := w.send(w.buf[:1+c]); err != nil {
			return n, err
		}
		n += c
		p = p[c:]
	}
	return n, nil
}

// ReadFrom sends what it reads from r until EOF, reading straight into the
// frame so that io.Copy needs no buffer of its own.
func (w *frameWriter) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	for {
		n, err := r.Read(w.buf[1:])
		if n > 0 {
			if err := w.send(w.buf[:1+n]); err != nil {
				return total, err
			}
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		if n == len(w.buf)-1 && len(w.buf) < 1+frameData {
			w.grow(1 + frameData)
		}
	}
}

func (w *frameWriter) grow(size int) {
	kind := w.buf[0]
	w.buf = make([]byte, size)
	w.buf[0] = kind
}

// messageReader reads whole messages into one buffer it reuses, where
// conn.Read allocates one per message. What read returns is valid until the
// next read.
type messageReader struct {
	conn *websocket.Conn
	buf  bytes.Buffer
}

func (r *messageReader) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	typ, msg, err := r.conn.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}
	r.buf.Reset()
	if _, err := r.buf.ReadFrom(msg); err != nil {
		return 0, nil, err
	}
	return typ, r.buf.Bytes(), nil
}
