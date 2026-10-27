package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// startTimeout bounds how long a connection that started a service waits
// for it to be ready; then it is dropped.
const startTimeout = time.Minute

// listenAddr parses a service's listen: "unix:PATH" or "tcp:PORT" (on
// localhost), as net.Listen takes them.
func listenAddr(listen string) (network, addr string, err error) {
	network, addr, ok := strings.Cut(listen, ":")
	switch {
	case !ok || addr == "":
	case network == "unix":
		return network, addr, nil
	case network == "tcp":
		return network, net.JoinHostPort("127.0.0.1", addr), nil
	}
	return "", "", fmt.Errorf("listen %q: want unix:PATH or tcp:PORT", listen)
}

// onDemand holds a service's socket and starts the service at the socket's
// first connection: a box whose turn never uses its database doesn't wait
// for it to start, nor keep its memory. The connections that come before
// the service is ready wait, and are relayed to it once it is.
//
// A unix socket stays fern-init's until the service is ready: a service
// such as Postgres replaces the socket file when it starts, and from then
// on connections reach it directly. A TCP port can't be shared, so it is let
// go at the first connection, before the service starts; a connection in
// between is refused.
type onDemand struct {
	name    string
	svc     service
	dir     string
	env     []string
	log     *slog.Logger
	network string
	addr    string
	ln      net.Listener
	own     os.FileInfo // a unix socket's file while it is fern-init's
	start   func() *supervisor

	mu      sync.Mutex
	started *supervisor // nil until the first connection
	conns   map[net.Conn]struct{}
	relays  sync.WaitGroup
	done    chan struct{} // closed when serve returns
}

// listenOnDemand takes svc's socket and serves it until stop.
func listenOnDemand(ctx context.Context, name string, svc service, dir string, env []string, log *slog.Logger) (*onDemand, error) {
	network, addr, err := listenAddr(svc.Listen)
	if err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("service %s: %w", name, err)
	}
	var own os.FileInfo
	if ul, ok := ln.(*net.UnixListener); ok {
		// Once the service has the path, it is the service's to remove.
		ul.SetUnlinkOnClose(false)
		if own, err = os.Stat(addr); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
	}
	o := &onDemand{
		name: name, svc: svc, dir: dir, env: env, log: log, network: network, addr: addr, ln: ln, own: own,
		start: func() *supervisor { return startService(ctx, name, svc, dir, env, log) },
		conns: make(map[net.Conn]struct{}), done: make(chan struct{}),
	}
	go o.serve(ctx)
	return o, nil
}

// serve accepts connections until the listener is closed: the first starts
// the service, and every one waits for it to be ready.
func (o *onDemand) serve(ctx context.Context) {
	defer close(o.done)
	var ready chan error
	for {
		conn, err := o.ln.Accept()
		if err != nil {
			return
		}
		o.mu.Lock()
		if o.started != nil && o.fromService(conn) {
			// The service checking whether it is up yet, as Postgres's start
			// script does: held, it would wait for itself.
			o.mu.Unlock()
			_ = conn.Close()
			continue
		}
		if o.started == nil {
			o.log.InfoContext(ctx, "starting service on demand", slog.String("service", o.name))
			if o.network == "tcp" {
				_ = o.ln.Close() // the service needs the port; serve ends after this connection
			}
			o.started = o.start()
			ready = make(chan error, 1)
			o.relays.Go(func() { ready <- o.waitReady(ctx) })
		}
		o.conns[conn] = struct{}{}
		o.mu.Unlock()
		o.relays.Go(func() { o.relay(ctx, conn, ready) })
	}
}

// waitReady waits until the service's ready check passes, then lets go of a
// unix socket, which the service has taken over.
func (o *onDemand) waitReady(ctx context.Context) error {
	defer func() {
		if o.network == "unix" {
			_ = o.ln.Close()
		}
	}()
	deadline := time.Now().Add(startTimeout)
	for {
		err := error(nil)
		switch {
		case o.network == "unix" && !o.replaced():
			// A ready check would connect to fern-init itself and wait.
			err = errors.New("its socket isn't there yet")
		case o.svc.Ready != "":
			err = shell(ctx, readyTimeout, o.svc.Ready, o.dir, o.env)
		}
		if err == nil {
			o.log.InfoContext(ctx, "service started on demand", slog.String("service", o.name))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s not ready after %s: %w", o.name, startTimeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryEvery):
		}
	}
}

// fromService says whether conn comes from a process of the started
// service: one in its process group, on a unix socket, which tells who is
// at the other end.
func (o *onDemand) fromService(conn net.Conn) bool {
	group := o.started.group.Load()
	pgid := peerGroup(conn)
	return group != 0 && int64(pgid) == group
}

// replaced says whether the service has put its own socket where
// fern-init's was.
func (o *onDemand) replaced() bool {
	fi, err := os.Stat(o.addr)
	return err == nil && !os.SameFile(fi, o.own)
}

// relay waits for the service, then copies conn to and from it.
func (o *onDemand) relay(ctx context.Context, conn net.Conn, ready chan error) {
	defer o.forget(conn)
	if err := o.wait(ctx, ready); err != nil {
		o.log.WarnContext(ctx, "dropping a connection", slog.String("service", o.name), slog.Any("error", err))
		return
	}
	up, err := (&net.Dialer{}).DialContext(ctx, o.network, o.addr)
	if err != nil {
		o.log.WarnContext(ctx, "relaying a connection", slog.String("service", o.name), slog.Any("error", err))
		return
	}
	defer func() { _ = up.Close() }()
	o.mu.Lock()
	o.conns[up] = struct{}{}
	o.mu.Unlock()
	defer o.forget(up)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(up, conn)
		closeWrite(up)
		close(done)
	}()
	_, _ = io.Copy(conn, up)
	closeWrite(conn)
	<-done
}

// wait returns the service's readiness, which every waiting connection
// reads: the first to come takes it and passes it on.
func (o *onDemand) wait(ctx context.Context, ready chan error) error {
	select {
	case err := <-ready:
		ready <- err
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *onDemand) forget(c net.Conn) {
	_ = c.Close()
	o.mu.Lock()
	delete(o.conns, c)
	o.mu.Unlock()
}

// closeWrite ends c's sending side, so its peer reads EOF and can answer
// before closing.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// stop closes the socket and the relayed connections, then stops the
// service if it started.
func (o *onDemand) stop(ctx context.Context) {
	err := o.ln.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		o.log.WarnContext(ctx, "closing a service's socket", slog.String("service", o.name), slog.Any("error", err))
	}
	<-o.done
	o.mu.Lock()
	for c := range o.conns {
		_ = c.Close()
	}
	started := o.started
	o.mu.Unlock()
	o.relays.Wait()
	if started != nil {
		started.stop(ctx)
	} else if o.network == "unix" {
		_ = os.Remove(o.addr) // still fern-init's own socket
	}
}

// isStarted says whether the service has started: until then it is ready,
// as its socket is.
func (o *onDemand) isStarted() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.started != nil
}
