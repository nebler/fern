package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveEcho is the service the tests start: after a moment, as a database
// takes, it listens on "unix:PATH" or "tcp:PORT", replacing a socket file
// there as Postgres does, writes PATH.ready (or ready in its directory) and
// answers every line with "svc:" and the line.
func serveEcho(listen string) {
	network, addr, err := listenAddr(listen)
	if err != nil {
		os.Exit(2)
	}
	// This runs as the service, in a process of its own, not as a test: it
	// is slow to start as a database is, on the real clock fern-init waits by.
	time.Sleep(200 * time.Millisecond)
	if network == "unix" {
		// As a start script checks whether its database is up: fern-init
		// must not hold this, or the service waits for itself.
		if c, err := (&net.Dialer{}).DialContext(context.Background(), network, addr); err == nil {
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			_, err := c.Read(make([]byte, 1))
			_ = c.Close()
			if errors.Is(err, os.ErrDeadlineExceeded) {
				os.Exit(3)
			}
		}
		_ = os.Remove(addr)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), network, addr)
	if err != nil {
		os.Exit(1)
	}
	_ = os.WriteFile("ready", nil, 0o600)
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		go func() {
			defer func() { _ = c.Close() }()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				_, _ = fmt.Fprintf(c, "svc:%s\n", sc.Text())
			}
		}()
	}
}

// ask sends line on a new connection to the socket and returns the answer.
func ask(t *testing.T, network, addr, line string) string {
	t.Helper()
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), network, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintln(c, line); err != nil {
		t.Fatal(err)
	}
	answer, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	return strings.TrimSpace(answer)
}

// A service with a socket starts at its first connection, which waits for
// it, and so do others that come meanwhile; it is ready until then.
func TestOnDemand(t *testing.T) {
	for _, network := range []string{"unix", "tcp"} {
		t.Run(network, func(t *testing.T) {
			dir := t.TempDir()
			listen := "unix:" + filepath.Join(dir, "svc.sock")
			if network == "tcp" {
				ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				_, port, _ := net.SplitHostPort(ln.Addr().String())
				_ = ln.Close()
				listen = "tcp:" + port
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			svc := service{Run: "FERN_TEST_ECHO=" + listen + " exec " + exe, Ready: "test -e ready", Listen: listen}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			o, err := listenOnDemand(ctx, "svc", svc, dir, []string{pathEnv}, startTimeout, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			// That nothing starts can't be waited for: a while on the real
			// clock, as fern-init's sockets and the service's process are real.
			time.Sleep(300 * time.Millisecond)
			if o.isStarted() {
				t.Fatal("started before a connection")
			}
			if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
				t.Fatal("the service ran before a connection")
			}
			_, addr, _ := listenAddr(listen)
			answers := make(chan string, 3)
			for i := range 3 {
				go func() {
					c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
					if err != nil {
						answers <- err.Error()
						return
					}
					defer func() { _ = c.Close() }()
					_, _ = fmt.Fprintf(c, "hi %d\n", i)
					a, _ := bufio.NewReader(c).ReadString('\n')
					answers <- strings.TrimSpace(a)
				}()
				if network == "tcp" {
					break // the port is the service's after the first
				}
			}
			n := 3
			if network == "tcp" {
				n = 1
			}
			for range n {
				if got := <-answers; !strings.HasPrefix(got, "svc:hi ") {
					t.Errorf("answer %q, want svc:hi …", got)
				}
			}
			if !o.isStarted() {
				t.Error("not started after a connection")
			}
			if got := ask(t, network, addr, "again"); got != "svc:again" {
				t.Errorf("answer once started %q, want svc:again", got)
			}
			cancel()
			o.stop(context.Background())
			if c, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), network, addr); err == nil {
				_ = c.Close()
				t.Error("the service still answers after stop")
			}
		})
	}
}

// One that never got a connection leaves no socket file behind.
func TestOnDemandUnused(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "svc.sock")
	ctx, cancel := context.WithCancel(t.Context())
	o, err := listenOnDemand(ctx, "svc", service{Run: "false", Listen: "unix:" + sock}, dir, []string{pathEnv}, startTimeout, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	o.stop(context.Background())
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file after stop: %v, want none", err)
	}
}

// A service that wasn't ready in time drops those who waited, and is waited
// for again at the next connection: its supervisor goes on starting it.
func TestOnDemandFailedThenAskedAgain(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "svc.sock")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Exits at once until the file ok is there.
	svc := service{Run: "[ -e ok ] || exit 1; FERN_TEST_ECHO=unix:" + sock + " exec " + exe, Ready: "test -e ready", Listen: "unix:" + sock}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Longer than the first pause between its starts, so that a wait sees one.
	o, err := listenOnDemand(ctx, "svc", svc, dir, []string{pathEnv}, 2*time.Second, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := bufio.NewReader(c).ReadString('\n'); !errors.Is(err, io.EOF) {
		t.Fatalf("a connection to a service that doesn't start: %v, want it dropped", err)
	}
	_ = c.Close()
	if why := o.supervisor().status().why(); !strings.Contains(why, "has exited") {
		t.Errorf("the service's status says %q, want that it has exited", why)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, "unix", sock, "later"); got != "svc:later" {
		t.Errorf("answer at the next connection %q, want svc:later", got)
	}
	cancel()
	o.stop(context.Background())
}

// One that never took its socket over leaves none behind either.
func TestOnDemandFailedLeavesNoSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "svc.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o, err := listenOnDemand(ctx, "svc", service{Run: "exit 1", Listen: "unix:" + sock}, dir, []string{pathEnv}, 100*time.Millisecond, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = bufio.NewReader(c).ReadString('\n') // dropped once the wait is over
	_ = c.Close()
	cancel()
	o.stop(context.Background())
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file after stop: %v, want none", err)
	}
}

// The port a service is previewed on is held on every address, as the
// preview comes to the box's address; any other only on localhost.
func TestOnDemandPreviewedPort(t *testing.T) {
	for _, previewed := range []bool{true, false} {
		t.Run(fmt.Sprintf("previewed=%v", previewed), func(t *testing.T) {
			free, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := free.Addr().(*net.TCPAddr).Port
			_ = free.Close()
			svc := service{Run: "true", Listen: fmt.Sprintf("tcp:%d", port)}
			if previewed {
				svc.Port = port
			}
			o, err := listenOnDemand(t.Context(), "web", svc, t.TempDir(), []string{pathEnv}, startTimeout, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer o.stop(context.Background())
			if got := o.ln.Addr().(*net.TCPAddr).IP.IsUnspecified(); got != previewed {
				t.Errorf("held on every address = %v, want %v (%s)", got, previewed, o.ln.Addr())
			}
		})
	}
}
