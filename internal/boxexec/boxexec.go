// Package boxexec is the exec protocol spoken by the CLI, the daemon and
// fern-init: one WebSocket per command.
//
// The client sends a JSON Request as a text message, then binary frames for
// stdin, stdin EOF and terminal resizes. The server sends binary frames for
// stdout and stderr, then the exit code as a JSON text message, and closes.
// Every binary frame starts with one byte saying what it carries.
//
// The server side is in server.go, the client side in client.go, and the
// daemon's relay between the two in proxy.go.
package boxexec

import (
	"context"
	"io"

	"github.com/coder/websocket"
)

// Port is the port fern-init serves exec on inside every box.
const Port = 7070

// Path is the URL path of fern-init's exec endpoint.
const Path = "/exec"

// URL returns the exec endpoint of the box serving at addr (host:port).
func URL(addr string) string { return "ws://" + addr + Path }

// Frame kinds: the first byte of every binary message.
const (
	frameStdin byte = iota
	frameStdout
	frameStderr
	frameStdinEOF
	frameResize
)

// MaxMessage is the largest message in the protocol; output frames are at
// most one pipe read. Every connection must be able to read it, which is more
// than the WebSocket library allows by default (32 KiB): use Accept and Dial,
// or call SetReadLimit(MaxMessage).
const MaxMessage = 1 << 20

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
