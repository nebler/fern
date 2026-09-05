// Package runterminal bridges an authorized browser WebSocket to the one PID 1
// shell in an exactly attested Docker container. It never creates Docker execs.
package runterminal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
	"golang.org/x/net/websocket"
)

type Opener interface {
	OpenTerminal(context.Context, taskstore.BackgroundRun, taskstore.BackgroundRunOwnership, string) (*taskenvdocker.Terminal, func(), error)
}

type Bridge struct{ opener Opener }

func New(opener Opener) (*Bridge, error) {
	if opener == nil {
		return nil, errors.New("terminal opener is required")
	}
	return &Bridge{opener: opener}, nil
}

func (b *Bridge) Serve(w http.ResponseWriter, r *http.Request, run taskstore.BackgroundRun, ownership taskstore.BackgroundRunOwnership, role string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !sameOrigin(r) {
		http.Error(w, "same-origin WebSocket required", http.StatusBadRequest)
		return
	}
	server := websocket.Server{Handshake: func(_ *websocket.Config, request *http.Request) error {
		if !sameOrigin(request) {
			return errors.New("cross-origin terminal rejected")
		}
		return nil
	}, Handler: func(connection *websocket.Conn) {
		connection.PayloadType = websocket.BinaryFrame
		terminal, release, err := b.opener.OpenTerminal(r.Context(), run, ownership, role)
		if err != nil {
			_ = websocket.Message.Send(connection, []byte("\r\nTerminal unavailable: "+err.Error()+"\r\n"))
			return
		}
		defer release()
		stopContext := context.AfterFunc(r.Context(), func() {
			_ = terminal.Close()
			_ = connection.Close()
		})
		defer stopContext()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(connection, terminal); done <- struct{}{} }()
		go func() { _, _ = io.Copy(terminal, connection); done <- struct{}{} }()
		<-done
	}}
	server.ServeHTTP(w, r)
}

func sameOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) != 1 || r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		return false
	}
	origin, err := url.Parse(values[0])
	return err == nil && origin.User == nil && origin.Host == r.Host && (origin.Scheme == "http" || origin.Scheme == "https") && origin.Path == ""
}
