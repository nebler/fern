package runterminal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

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
