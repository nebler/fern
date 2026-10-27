package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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
			o, err := listenOnDemand(ctx, "svc", svc, dir, []string{pathEnv}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
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
	o, err := listenOnDemand(ctx, "svc", service{Run: "false", Listen: "unix:" + sock}, dir, []string{pathEnv}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	o.stop(context.Background())
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket file after stop: %v, want none", err)
	}
}
