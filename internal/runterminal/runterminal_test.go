package runterminal

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

func TestForwardAndWaitCopiesBothDirectionsThenDisconnectsBothSides(t *testing.T) {
	browser, browserPeer := net.Pipe()
	terminal, terminalPeer := net.Pipe()
	defer browserPeer.Close()
	defer terminalPeer.Close()
	done := make(chan struct{})
	go func() {
		forwardAndWait(browser, terminal)
		close(done)
	}()

	assertForwarded := func(t *testing.T, source net.Conn, destination net.Conn, value string) {
		t.Helper()
		writeDone := make(chan error, 1)
		go func() {
			_, err := io.WriteString(source, value)
			writeDone <- err
		}()
		got := make([]byte, len(value))
		if _, err := io.ReadFull(destination, got); err != nil {
			t.Fatal(err)
		}
		if err := <-writeDone; err != nil || string(got) != value {
			t.Fatalf("forwarded %q with write error %v", got, err)
		}
	}
	assertForwarded(t, browserPeer, terminalPeer, "command")
	assertForwarded(t, terminalPeer, browserPeer, "output")

	if err := browserPeer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forwarding goroutines did not both exit after disconnect")
	}
	if _, err := terminalPeer.Read(make([]byte, 1)); err == nil {
		t.Fatal("terminal side remained connected")
	}
}

func TestBridgeRejectsNonGetAndCrossOriginBeforeAttach(t *testing.T) {
	opener := &fakeOpener{}
	bridge, err := New(opener)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRecorder()
	bridge.Serve(post, httptest.NewRequest(http.MethodPost, "https://fern.example/terminal", nil), taskstore.BackgroundRun{}, taskstore.BackgroundRunOwnership{}, taskenvdocker.ShellRoleHuman)
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET" || opener.called {
		t.Fatalf("POST status=%d allow=%q called=%t", post.Code, post.Header().Get("Allow"), opener.called)
	}
	request := httptest.NewRequest(http.MethodGet, "https://fern.example/terminal", nil)
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Origin", "https://other.example")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	cross := httptest.NewRecorder()
	bridge.Serve(cross, request, taskstore.BackgroundRun{}, taskstore.BackgroundRunOwnership{}, taskenvdocker.ShellRoleHuman)
	if cross.Code != http.StatusBadRequest || opener.called {
		t.Fatalf("cross-origin status=%d called=%t", cross.Code, opener.called)
	}
}

type fakeOpener struct{ called bool }

func (f *fakeOpener) OpenTerminal(context.Context, taskstore.BackgroundRun, taskstore.BackgroundRunOwnership, string) (*taskenvdocker.Terminal, func(), error) {
	f.called = true
	return nil, nil, errors.New("unexpected attach")
}
