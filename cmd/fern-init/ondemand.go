package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
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

// demandPhase is where a service started on demand is.
type demandPhase int

const (
	// demandListening: fern-init holds its socket, and nothing has connected.
	// The service doesn't run, and counts as ready: its socket is.
	demandListening demandPhase = iota
	// demandStarting: a connection started it, or asked for it again after
	// demandFailed. Connections wait until it is ready.
	demandStarting
	// demandReady: it has its socket. The connections that waited are relayed
	// to it, and later ones reach it without fern-init.
	demandReady
	// demandFailed: it wasn't ready within the time allowed, and those waiting
	// were dropped. Its supervisor goes on starting it. On a unix socket that
	// is still fern-init's, the next connection waits for it again; a TCP
	// port was let go, so later connections are the service's or refused.
	demandFailed
)

// attempt is one wait for the service to be ready, shared by every
// connection that came meanwhile.
type attempt struct {
	done chan struct{} // closed once err is set
	err  error
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
	own     os.FileInfo   // a unix socket's file while it is fern-init's
	timeout time.Duration // how long connections wait for the service to be ready
	start   func() *supervisor
	relays  sync.WaitGroup
	done    chan struct{} // closed when serve returns

	mu    sync.Mutex
	phase demandPhase
	sup   *supervisor // nil while demandListening
	wait  *attempt    // the last wait for it; nil while demandListening
	conns map[net.Conn]struct{}
}

// listenOnDemand takes svc's socket and serves it until stop. Connections
// wait at most timeout for the service to be ready.
func listenOnDemand(ctx context.Context, name string, svc service, dir string, env []string, timeout time.Duration, log *slog.Logger) (*onDemand, error) {
	network, addr, err := listenAddr(svc.Listen)
	if err != nil {
		return nil, err
	}
	// The preview reaches a box's port at the box's address, not localhost:
	// the port a service is previewed on is held on every address, or a
	// page's first request would find nothing and never start it. The
	// relay still reaches the service on localhost.
	bind := addr
	if network == "tcp" && svc.Port != 0 && svc.Listen == "tcp:"+strconv.Itoa(svc.Port) {
		bind = ":" + strconv.Itoa(svc.Port)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, network, bind)
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
		name: name, svc: svc, dir: dir, env: env, log: log, network: network, addr: addr, ln: ln, own: own, timeout: timeout,
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
	for {
		conn, err := o.ln.Accept()
		if err != nil {
			return
		}
		o.mu.Lock()
		if o.phase != demandListening && o.fromService(conn) {
			// The service checking whether it is up yet, as Postgres's start
			// script does: held, it would wait for itself.
			o.mu.Unlock()
			_ = conn.Close()
			continue
		}
		switch o.phase {
		case demandListening:
			o.log.InfoContext(ctx, "starting service on demand", slog.String("service", o.name))
			if o.network == "tcp" {
				_ = o.ln.Close() // the service needs the port; serve ends after this connection
			}
			o.sup = o.start()
			o.await(ctx)
		case demandFailed:
			// Its supervisor has gone on starting it: it gets as long again.
			o.await(ctx)
		case demandStarting, demandReady:
		}
		wait := o.wait
		o.conns[conn] = struct{}{}
		o.mu.Unlock()
		o.relays.Go(func() { o.relay(ctx, conn, wait) })
	}
}

// await begins a wait for the service to be ready, which ends in
// demandReady or demandFailed. The caller holds mu.
func (o *onDemand) await(ctx context.Context) {
	a := &attempt{done: make(chan struct{})}
	o.phase, o.wait = demandStarting, a
	o.relays.Go(func() {
		err := o.waitReady(ctx)
		o.mu.Lock()
		a.err, o.phase = err, demandReady
		if err != nil {
			o.phase = demandFailed
		}
		o.mu.Unlock()
		close(a.done)
		// A unix socket the service has taken over reaches fern-init no more.
		// One it hasn't stays, for the next connection to wait again.
		if o.network == "unix" && (err == nil || o.replaced()) {
			_ = o.ln.Close()
		}
	})
}

// waitReady waits until the service has its socket and its ready check
// passes, for at most o.timeout.
func (o *onDemand) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(o.timeout)
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
			return fmt.Errorf("service %s not ready after %s%s: %w", o.name, o.timeout, o.supervisor().status().why(), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryEvery):
		}
	}
}

// supervisor returns the service's supervisor, or nil while nothing has
// connected.
func (o *onDemand) supervisor() *supervisor {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sup
}

// fromService says whether conn comes from a process of the started
// service: one in its process group, on a unix socket, which tells who is
// at the other end.
func (o *onDemand) fromService(conn net.Conn) bool {
	group := o.sup.group.Load()
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
func (o *onDemand) relay(ctx context.Context, conn net.Conn, wait *attempt) {
	defer o.forget(conn)
	if err := wait.result(ctx); err != nil {
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

// result waits for the attempt's end and returns how it went.
func (a *attempt) result(ctx context.Context) error {
	select {
	case <-a.done:
		return a.err
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
	sup := o.sup
	o.mu.Unlock()
	o.relays.Wait()
	if sup != nil {
		sup.stop(ctx)
	}
	// A socket file that is still fern-init's own: nothing connected, or the
	// service never took it over. One the service made is its to remove.
	if o.network == "unix" && !o.replaced() {
		_ = os.Remove(o.addr)
	}
}

// isStarted says whether the service has been started: until then it is
// ready, as its socket is.
func (o *onDemand) isStarted() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.phase != demandListening
}
