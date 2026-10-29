package boxexec_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/nebler/fern/internal/boxexec"
)

// BenchmarkRequestJSON isolates request encoding from network and process costs.
func BenchmarkRequestJSON(b *testing.B) {
	req := boxexec.Request{Argv: []string{"claude", "--input-format", "stream-json", "--output-format", "stream-json"}, Dir: "/var/lib/fern/worktrees/sample/thread", Environ: []string{"CLAUDE_CODE_ENTRYPOINT=sdk-ts", "CLAUDE_AGENT_SDK_VERSION=0.3.276"}}
	for b.Loop() {
		data, err := json.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
		var decoded boxexec.Request
		if err := json.Unmarshal(data, &decoded); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpenExec measures retained Go heap and goroutines across the whole
// in-process path while commands wait for input. Run with -benchtime=1x.
func BenchmarkOpenExec(b *testing.B) {
	const count = 32
	for _, target := range targets(b) {
		b.Run(target.name, func(b *testing.B) {
			for b.Loop() {
				benchmarkOpenExec(b, target.url, count)
			}
		})
	}
}

func benchmarkOpenExec(b *testing.B, url string, count int) {
	b.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(b.Context())
	var wg sync.WaitGroup
	ready := make(chan struct{}, count)
	done := make(chan error, count)
	var inputs []*io.PipeWriter
	defer func() {
		cancel()
		for _, in := range inputs {
			_ = in.Close()
		}
		wg.Wait()
	}()
	for range count {
		conn, err := boxexec.Dial(ctx, url, token, nil)
		if err != nil {
			b.Fatal(err)
		}
		in, out := io.Pipe()
		inputs = append(inputs, out)
		wg.Go(func() {
			defer func() { _ = conn.CloseNow(); _ = in.Close() }()
			var once sync.Once
			stdout := writerFunc(func(p []byte) (int, error) {
				once.Do(func() { ready <- struct{}{} })
				return len(p), nil
			})
			_, err := boxexec.Run(ctx, conn, boxexec.Request{Argv: []string{"sh", "-c", "echo ready; cat >/dev/null"}}, boxexec.Streams{Stdin: in, Stdout: stdout, Stderr: io.Discard})
			done <- err
		})
	}
	for range count {
		select {
		case <-ready:
		case err := <-done:
			b.Fatalf("exec ended before ready: %v", err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/float64(count), "heap-B/exec")
	b.ReportMetric(float64(runtime.NumGoroutine()-goroutines)/float64(count), "goroutines/exec")
}

// BenchmarkRunStartup includes both the handshake and a quiet process's lifetime.
func BenchmarkRunStartup(b *testing.B) {
	for _, target := range targets(b) {
		b.Run(target.name, func(b *testing.B) {
			for b.Loop() {
				conn, err := boxexec.Dial(b.Context(), target.url, token, nil)
				if err != nil {
					b.Fatal(err)
				}
				code, err := boxexec.Run(b.Context(), conn, boxexec.Request{Argv: []string{"true"}}, boxexec.Streams{Stdout: io.Discard, Stderr: io.Discard})
				_ = conn.CloseNow()
				if err != nil || code != 0 {
					b.Fatalf("Run(true) = %d, %v", code, err)
				}
			}
		})
	}
}

// BenchmarkProcessStart isolates Linux process creation from the protocol.
func BenchmarkProcessStart(b *testing.B) {
	for b.Loop() {
		if err := exec.CommandContext(b.Context(), "true").Run(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHandshake measures a connection without starting a command.
func BenchmarkHandshake(b *testing.B) {
	url, _ := newBox(b)
	hc := &http.Client{Transport: &http.Transport{}}
	defer hc.CloseIdleConnections()
	for b.Loop() {
		conn, err := boxexec.Dial(b.Context(), url, token, hc)
		if err != nil {
			b.Fatal(err)
		}
		_ = conn.CloseNow()
	}
}

// BenchmarkRunThroughput measures streaming a command's output to the client.
func BenchmarkRunThroughput(b *testing.B) {
	const size = 64 << 20
	req := boxexec.Request{Argv: []string{"head", "-c", strconv.Itoa(size), "/dev/zero"}}
	for _, target := range targets(b) {
		b.Run(target.name, func(b *testing.B) {
			b.SetBytes(size)
			for b.Loop() {
				conn, err := boxexec.Dial(b.Context(), target.url, token, nil)
				if err != nil {
					b.Fatalf("Dial(%s) error: %v", target.url, err)
				}
				var n int
				count := writerFunc(func(p []byte) (int, error) { n += len(p); return len(p), nil })
				code, err := boxexec.Run(b.Context(), conn, req, boxexec.Streams{Stdout: count, Stderr: io.Discard})
				_ = conn.CloseNow()
				if err != nil || code != 0 || n != size {
					b.Fatalf("Run(%q) = %d, %v with %d bytes of output, want 0, nil with %d", req.Argv, code, err, n, size)
				}
			}
		})
	}
}
