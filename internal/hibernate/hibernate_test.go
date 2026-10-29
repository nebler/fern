package hibernate_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/hibernate"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const idle = 30 * time.Second

var claudeArgv = []string{"claude", "--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--session-id=s1"}

// fakeClaude is Claude in a box, as Exec runs it: it answers the SDK's
// control requests, and each user message with a turn (init, assistant,
// result). A user message saying "task" starts a background task, which
// ends when the test sends a line on out. A signal stops it with 143, the
// end of its stdin with 0.
type fakeClaude struct {
	mu     sync.Mutex
	argvs  [][]string
	inputs [][]string // the type (and subtype) of every line each run read
	out    chan string
}

func newFakeClaude() *fakeClaude { return &fakeClaude{out: make(chan string)} }

func (f *fakeClaude) exec(ctx context.Context, req boxexec.Request, s boxexec.Streams) (int, error) {
	f.mu.Lock()
	run := len(f.argvs)
	f.argvs = append(f.argvs, req.Argv)
	f.inputs = append(f.inputs, nil)
	f.mu.Unlock()
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(s.Stdin)
		for sc.Scan() {
			lines <- bytes.Clone(sc.Bytes())
		}
	}()
	write := func(format string, args ...any) { _, _ = fmt.Fprintf(s.Stdout, format+"\n", args...) }
	stubborn := false // ignores signals
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return 0, nil
			}
			var m struct {
				Type      string
				RequestID string `json:"request_id"`
				Request   struct{ Subtype string }
				Message   struct{ Content string }
			}
			if err := json.Unmarshal(line, &m); err != nil {
				return 1, err
			}
			f.mu.Lock()
			f.inputs[run] = append(f.inputs[run], strings.TrimSuffix(m.Type+"/"+m.Request.Subtype, "/"))
			f.mu.Unlock()
			switch m.Type {
			case "control_request":
				write(`{"type":"control_response","response":{"subtype":"success","request_id":%q}}`, m.RequestID)
			case "control_response":
				write(`{"type":"result","subtype":"success"}`) // the permission came: the turn ends
			case "user":
				write(`{"type":"system","subtype":"init","session_id":"s2"}`)
				write(`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`)
				if m.Message.Content == "task" {
					write(`{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"t1"}]}`)
				}
				switch m.Message.Content {
				case "ask":
					// A permission prompt, which holds the turn open.
					write(`{"type":"control_request","request_id":"p1","request":{"subtype":"can_use_tool"}}`)
					continue
				case "hold":
					continue // still working
				case "stubborn":
					stubborn = true
				}
				write(`{"type":"result","subtype":"success"}`)
			}
		case line := <-f.out:
			write("%s", line)
		case <-s.Signal:
			if !stubborn {
				return 143, nil
			}
		case <-ctx.Done():
			return 1, ctx.Err()
		}
	}
}

func (f *fakeClaude) runs() ([][]string, [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.argvs...), append([][]string(nil), f.inputs...)
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		var m struct{ Type, Subtype string }
		_ = json.Unmarshal([]byte(l), &m)
		out = append(out, strings.TrimSuffix(m.Type+"/"+m.Subtype, "/"))
	}
	return out
}

// session is a Run in progress, with the SDK's end of it.
type session struct {
	sdk    *io.PipeWriter
	stdout *syncBuffer
	sigs   chan os.Signal
	done   chan result
}

type result struct {
	code int
	err  error
}

func start(f *fakeClaude, argv []string) *session {
	pr, pw := io.Pipe()
	s := &session{sdk: pw, stdout: &syncBuffer{}, sigs: make(chan os.Signal, 1), done: make(chan result, 1)}
	go func() {
		code, err := hibernate.Run(context.Background(), f.exec, boxexec.Request{Argv: argv},
			boxexec.Streams{Stdin: pr, Stdout: s.stdout, Stderr: io.Discard, Signal: s.sigs}, idle)
		_ = pr.Close()
		s.done <- result{code, err}
	}()
	return s
}

func (s *session) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(s.sdk, line+"\n"); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
}

func (s *session) end(t *testing.T) result {
	t.Helper()
	_ = s.sdk.Close()
	return <-s.done
}

const (
	initialize = `{"type":"control_request","request_id":"i1","request":{"subtype":"initialize","hooks":{}}}`
	mcp        = `{"type":"control_request","request_id":"m1","request":{"subtype":"mcp_set_servers","servers":{}}}`
	usage      = `{"type":"control_request","request_id":"u1","request":{"subtype":"get_context_usage"}}`
)

func user(text string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, text)
}

// TestStopAndResume: an idle Claude stops; the next message starts it again
// in its session, after the setup, and the SDK sees one conversation.
func TestStopAndResume(t *testing.T) {
	stream := []string{"claude", "--output-format", "stream-json", "--verbose", "--input-format", "stream-json"}
	for _, tc := range []struct {
		name string
		args []string // after stream's
	}{
		{"new session", []string{"--session-id=s1"}},
		{"fork", []string{"--resume=s0", "--fork-session", "--resume-session-at=m9", "--model", "haiku"}},
		{"values apart", []string{"--resume", "s0", "--model", "haiku"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeClaude()
				argv := append(append([]string(nil), stream...), tc.args...)
				s := start(f, argv)
				s.send(t, initialize)
				s.send(t, mcp)
				s.send(t, usage)
				s.send(t, user("one"))
				time.Sleep(idle - time.Second)
				synctest.Wait()
				if argvs, _ := f.runs(); len(argvs) != 1 {
					t.Fatalf("before the idle time: %d runs, want 1", len(argvs))
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				time.Sleep(time.Hour) // stopped: nothing happens
				s.send(t, user("two"))
				if got := s.end(t); got != (result{}) {
					t.Errorf("Run = %+v, want exit 0", got)
				}
				argvs, inputs := f.runs()
				resumed := append([]string(nil), stream...)
				if slices.Contains(tc.args, "--model") {
					resumed = append(resumed, "--model", "haiku")
				}
				resumed = append(resumed, "--resume=s2")
				if diff := cmp.Diff([][]string{argv, resumed}, argvs); diff != "" {
					t.Errorf("argv per run (-want +got):\n%s", diff)
				}
				wantIn := [][]string{
					{"control_request/initialize", "control_request/mcp_set_servers", "control_request/get_context_usage", "user"},
					{"control_request/initialize", "control_request/mcp_set_servers", "user"},
				}
				if diff := cmp.Diff(wantIn, inputs); diff != "" {
					t.Errorf("lines per run (-want +got):\n%s", diff)
				}
				wantOut := []string{
					"control_response", "control_response", "control_response",
					"system/init", "assistant", "result/success", "system/init", "assistant", "result/success",
				}
				if diff := cmp.Diff(wantOut, s.stdout.lines()); diff != "" {
					t.Errorf("what the SDK read (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestStaysWhileBusy: Claude isn't stopped mid-turn, while waiting for a
// permission, or while a background task runs.
func TestStaysWhileBusy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lines  []string
		finish func(t *testing.T, s *session, f *fakeClaude)
	}{
		{"turn without a result", []string{initialize, user("hold")}, nil},
		{"permission asked", []string{initialize, user("ask")}, func(t *testing.T, s *session, _ *fakeClaude) {
			s.send(t, `{"type":"control_response","response":{"subtype":"success","request_id":"p1"}}`)
		}},
		{"background task", []string{initialize, user("task")}, func(_ *testing.T, _ *session, f *fakeClaude) {
			f.out <- `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeClaude()
				s := start(f, claudeArgv)
				for _, l := range tc.lines {
					s.send(t, l)
				}
				time.Sleep(3 * idle)
				synctest.Wait()
				if argvs, inputs := f.runs(); len(argvs) != 1 {
					t.Fatalf("%d runs, want 1 (inputs %v)", len(argvs), inputs)
				}
				if tc.finish == nil {
					s.sigs <- syscall.SIGTERM
					if got := <-s.done; got.code != 143 {
						t.Errorf("Run = %+v, want Claude's exit 143", got)
					}
					return
				}
				// Once it is done, it stops after the idle time.
				tc.finish(t, s, f)
				time.Sleep(2 * idle)
				s.send(t, user("again"))
				if argvs, _ := f.runs(); len(argvs) != 2 {
					t.Errorf("%d runs after it finished, want 2", len(argvs))
				}
				if got := s.end(t); got != (result{}) {
					t.Errorf("Run = %+v, want exit 0", got)
				}
			})
		})
	}
}

// TestEnd: the SDK ends Claude, stopped or running, and Run returns its
// exit code.
func TestEnd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopped bool
		signal  bool
		want    int
	}{
		{"stdin ends, running", false, false, 0},
		{"signal, running", false, true, 143},
		{"stdin ends, stopped", true, false, 0},
		{"signal, stopped", true, true, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeClaude()
				s := start(f, claudeArgv)
				s.send(t, initialize)
				s.send(t, user("one"))
				if tc.stopped {
					time.Sleep(2 * idle)
					synctest.Wait()
				}
				var got result
				if tc.signal {
					s.sigs <- syscall.SIGTERM
					got = <-s.done
					_ = s.sdk.Close()
				} else {
					got = s.end(t)
				}
				if got.code != tc.want || got.err != nil {
					t.Errorf("Run = %+v, want exit %d", got, tc.want)
				}
				if argvs, _ := f.runs(); len(argvs) != 1 {
					t.Errorf("%d runs, want 1", len(argvs))
				}
			})
		})
	}
}

// TestOtherCommands run as they are, without stopping.
func TestOtherCommands(t *testing.T) {
	for _, argv := range [][]string{
		{"claude", "-p", "write a commit message"},
		{"claude", "--input-format", "stream-json"},
		{"sh", "-c", "claude --input-format stream-json --output-format stream-json"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFakeClaude()
				s := start(f, argv)
				s.send(t, initialize)
				s.send(t, user("one"))
				time.Sleep(2 * idle)
				synctest.Wait()
				if got := s.end(t); got != (result{}) {
					t.Errorf("Run = %+v, want exit 0", got)
				}
				if argvs, _ := f.runs(); len(argvs) != 1 {
					t.Errorf("%d runs, want 1", len(argvs))
				}
			})
		})
	}
}

// TestStubbornClaude: a Claude that doesn't exit on SIGTERM is cut off, and
// the SDK's next line, which waited, goes to a new one.
func TestStubbornClaude(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeClaude()
		s := start(f, claudeArgv)
		s.send(t, initialize)
		s.send(t, user("stubborn"))
		time.Sleep(idle + time.Second)
		s.send(t, user("two")) // waits while the first is stopping
		if argvs, _ := f.runs(); len(argvs) != 1 {
			t.Fatalf("while stopping: %d runs, want 1", len(argvs))
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if got := s.end(t); got != (result{}) {
			t.Errorf("Run = %+v, want exit 0", got)
		}
		_, inputs := f.runs()
		want := [][]string{{"control_request/initialize", "user"}, {"control_request/initialize", "user"}}
		if diff := cmp.Diff(want, inputs); diff != "" {
			t.Errorf("lines per run (-want +got):\n%s", diff)
		}
	})
}
