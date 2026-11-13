package boxexec_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/boxexec"
)

// The frame kinds, as the protocol has them (boxexec.go): the first byte of
// every binary message.
const (
	kindStdin byte = iota
	kindStdout
	kindStderr
	kindStdinEOF
	kindResize
	kindSignal
	kindAck
	kindLost
	kindGone
)

// messages splits fuzz input into WebSocket messages: a byte saying how
// long the next one is (up to 15), then the message, whose first byte is
// its kind.
func messages(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := min(int(data[0]%16), len(data)-1)
		out = append(out, data[1:1+n])
		data = data[1+n:]
	}
	return out
}

// encode is the fuzz input that messages splits into msgs.
func encode(msgs ...[]byte) []byte {
	var out []byte
	for _, m := range msgs {
		out = append(append(out, byte(len(m))), m...)
	}
	return out
}

// drain reads conn until it ends, then closes done.
func drain(ctx context.Context, conn *websocket.Conn, done chan<- struct{}) {
	defer close(done)
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

func count(kind byte, n uint64) []byte { return binary.BigEndian.AppendUint64([]byte{kind}, n) }

// What a box's fern-init sends, whatever it is, reaches the client's
// streams as it says, frame for frame: the stdout and stderr frames' data
// and nothing else, a lost-output note for each frame that says output was
// lost, and the end at a frame that says the session is gone. Acks of the
// client's stdin, however wrong, change none of it. RunSession is what the
// daemon and the CLI run a box's commands with.
func FuzzRunSessionFrames(f *testing.F) {
	f.Add(encode([]byte{kindStdout, 'h', 'i'}, []byte{kindStderr, 'e'}))
	f.Add(encode(count(kindAck, 1), []byte{kindStdout, 'o'}, count(kindAck, 2)))
	f.Add(encode(count(kindAck, 1<<40), []byte{kindStdout}, count(kindLost, 3)))
	f.Add(encode([]byte{kindStdout, 'a'}, []byte{kindGone}, []byte{kindStdout, 'b'}))
	f.Add(encode([]byte{}, []byte{kindResize, 1, 2, 3, 4}, []byte{99, 'x'}, []byte{kindAck}))

	msgs := make(chan [][]byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := boxexec.Accept(w, r)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		var req boxexec.Request
		if err := wsjson.Read(r.Context(), conn, &req); err != nil {
			return
		}
		// What the client sends is read until it closes, which answers its
		// close message.
		read := make(chan struct{})
		go drain(r.Context(), conn, read)
		defer func() { <-read }()
		for _, m := range <-msgs {
			if conn.Write(r.Context(), websocket.MessageBinary, m) != nil {
				return
			}
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"code":3}`))
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	f.Fuzz(func(t *testing.T, data []byte) {
		sent := messages(data)
		var wantOut, wantErr bytes.Buffer
		wantCode, wantGone := 3, false
		for _, m := range sent {
			if len(m) == 0 {
				continue
			}
			switch m[0] {
			case kindStdout:
				wantOut.Write(m[1:])
			case kindStderr:
				wantErr.Write(m[1:])
			case kindLost:
				if len(m) == 9 {
					wantErr.WriteString("[… output lost while Fern restarted …]\n")
				}
			}
			if m[0] == kindGone {
				wantCode, wantGone = 0, true
				break
			}
		}

		ctx := t.Context()
		conn, err := boxexec.Dial(ctx, url, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		msgs <- sent
		var stdout, stderr bytes.Buffer
		code, err := boxexec.RunSession(ctx, conn, boxexec.Request{Argv: []string{"cat"}, Session: "s"},
			boxexec.Streams{Stdin: strings.NewReader("some stdin"), Stdout: &stdout, Stderr: &stderr}, boxexec.Resume{})
		if gone := errors.Is(err, boxexec.ErrSessionGone); gone != wantGone || (!gone && err != nil) || code != wantCode {
			t.Errorf("RunSession() = %d, %v; want %d, the session gone: %t", code, err, wantCode, wantGone)
		}
		if diff := cmp.Diff(wantOut.String(), stdout.String()); diff != "" {
			t.Errorf("stdout (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(wantErr.String(), stderr.String()); diff != "" {
			t.Errorf("stderr (-want +got):\n%s", diff)
		}
	})
}

// Whatever a client sends a box's fern-init, the command ends and the
// client hears how: and the command reads, once and in order, the data of
// the stdin frames up to the first end of stdin. A frame that isn't stdin
// changes none of it, but an ack: one of output the client hasn't had lets
// the box drop it, as asked. Signals aren't sent, which would kill it.
func FuzzServerFrames(f *testing.F) {
	f.Add(encode([]byte{kindStdin, 'h', 'i'}, []byte{kindStdin, '!'}), true)
	f.Add(encode([]byte{kindStdin, 'h', 'i'}, []byte{kindStdinEOF}, []byte{kindStdin, 'x'}), false)
	f.Add(encode([]byte{kindStdin}, []byte{kindResize, 0, 24, 0, 80}, count(kindAck, 1), []byte{kindStdout, 'o'}), true)
	f.Add(encode(count(kindAck, 1<<40), []byte{kindStdin, 'a'}, []byte{kindGone}, count(kindLost, 0)), true)
	f.Add(encode([]byte{kindAck}, []byte{}, []byte{200, 1}), false)

	box := boxexec.NewServer("token", f.TempDir(), []string{"PATH=/usr/local/bin:/usr/bin:/bin"}, slog.New(slog.DiscardHandler))
	// A session whose client is gone stops at once.
	box.Grace = time.Millisecond
	srv := httptest.NewServer(box)
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	var n int

	f.Fuzz(func(t *testing.T, data []byte, session bool) {
		var want bytes.Buffer
		acked, ended := false, false
		var sent [][]byte
		for _, m := range messages(data) {
			if len(m) > 0 && m[0] == kindSignal {
				continue
			}
			sent = append(sent, m)
			if len(m) == 0 {
				continue
			}
			switch m[0] {
			case kindStdin:
				if !ended {
					want.Write(m[1:])
				}
			case kindStdinEOF:
				ended = true
			case kindAck:
				acked = acked || session
			}
		}

		ctx := t.Context()
		conn, err := boxexec.Dial(ctx, url, "token", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.CloseNow() }()
		req := boxexec.Request{Argv: []string{"cat"}}
		if session {
			n++
			req.Session = "fuzz-" + strconv.Itoa(n)
		}
		if err := wsjson.Write(ctx, conn, req); err != nil {
			t.Fatal(err)
		}
		for _, m := range append(sent, []byte{kindStdinEOF}) {
			if err := conn.Write(ctx, websocket.MessageBinary, m); err != nil {
				t.Fatalf("send %v: %v", m, err)
			}
		}
		var got bytes.Buffer
		for {
			typ, msg, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("the command's end never came: %v", err)
			}
			if typ == websocket.MessageText {
				if strings.TrimSpace(string(msg)) != `{"code":0}` {
					t.Errorf("the end = %s, want code 0", msg)
				}
				break
			}
			if len(msg) > 0 && msg[0] == kindStdout {
				got.Write(msg[1:])
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
		if acked {
			return
		}
		if diff := cmp.Diff(want.String(), got.String()); diff != "" {
			t.Errorf("what the command read (-want +got):\n%s", diff)
		}
	})
}
