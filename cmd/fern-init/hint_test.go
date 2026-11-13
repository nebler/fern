package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/setup"
)

// The hint names the services, is replaced at every start, and leaves a
// CLAUDE.md someone else wrote alone.
func TestWriteHint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if err := writeHint(dir, hint{services: map[string]service{"redis": {}, "postgres": {}}, preview: "http://{port}-demo.localhost:30780/"}); err != nil {
		t.Fatalf("writeHint() error: %v", err)
	}
	if got := read(); !strings.Contains(got, "postgres, redis") || !strings.HasPrefix(got, hintMark) ||
		!strings.Contains(got, "http://5173-demo.localhost:30780/") {
		t.Errorf("hint = %q, want it marked, naming postgres, redis and a preview's link", got)
	}
	if err := writeHint(dir, hint{}); err != nil {
		t.Fatalf("writeHint() again error: %v", err)
	}
	if got := read(); strings.Contains(got, "postgres") || strings.Contains(got, "Cloudflare") || strings.Contains(got, "setup script") {
		t.Errorf("hint after the services went = %q, want it replaced, and nothing on Cloudflare or a setup script", got)
	}
	if got := read(); !strings.Contains(got, "use the `commit` tool") || !strings.Contains(got, "never in /tmp") || strings.Contains(got, "mise install") ||
		strings.Contains(got, "can't be read") {
		t.Errorf("hint = %q, want it to name the commit tool and say that /tmp doesn't last, and nothing on tools or a broken services.yaml", got)
	}
	if err := writeHint(dir, hint{unreadable: "services.yaml: unknown field \"servces\""}); err != nil {
		t.Fatalf("writeHint() with a services.yaml it can't read error: %v", err)
	}
	if got := read(); !strings.Contains(got, `The fault: services.yaml: unknown field "servces".`) || !strings.Contains(got, "every Bash call fails") {
		t.Errorf("hint for a services.yaml that can't be read = %q, want the fault and what it means", got)
	}
	if err := writeHint(dir, hint{tools: true}); err != nil {
		t.Fatalf("writeHint() with tools error: %v", err)
	}
	if got := read(); !strings.Contains(got, "run `mise install`") {
		t.Errorf("hint for a repository with tools = %q, want it to say what to do when one is missing", got)
	}
	if err := writeHint(dir, hint{short: "4 CPUs (the box has 2)"}); err != nil {
		t.Fatalf("writeHint() for a box with less than asked error: %v", err)
	}
	if got := read(); !strings.Contains(got, "asks for more than this server gives a box: 4 CPUs (the box has 2).") || !strings.Contains(got, "--box-max-cpus") {
		t.Errorf("hint for a box with less than asked = %q, want it to say what is short and how the person raises it", got)
	}
	if err := writeHint(dir, hint{setup: ".fern/setup"}); err != nil {
		t.Fatalf("writeHint() with a setup script error: %v", err)
	}
	if got := read(); !strings.Contains(got, "a setup script, .fern/setup") || !strings.Contains(got, "run it yourself") {
		t.Errorf("hint for a repository with a setup script = %q, want it to name the script and say when to run it", got)
	}
	if err := writeHint(dir, hint{cloudflare: true}); err != nil {
		t.Fatalf("writeHint() for Cloudflare error: %v", err)
	}
	if got := read(); !strings.Contains(got, "deploys to Cloudflare") || !strings.Contains(got, ".wrangler/state") {
		t.Errorf("hint for a Cloudflare repository = %q, want it to say so, and where D1 and R2 keep their data", got)
	}
	if err := os.WriteFile(path, []byte("my own notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeHint(dir, hint{}); err != nil {
		t.Fatalf("writeHint() over someone's file error: %v", err)
	}
	if got := read(); got != "my own notes\n" {
		t.Errorf("CLAUDE.md = %q after writeHint, want someone's own notes kept", got)
	}
}

// Claude is told what its repository asks for its box and doesn't get.
func TestShortOf(t *testing.T) {
	tests := []struct {
		name  string
		asked setup.Limits
		given string
		want  string
	}{
		{name: "nothing asked", given: "2:2048:4096"},
		{name: "asked for what it gets", asked: setup.Limits{CPUs: 2, Memory: 2048}, given: "2:2048:4096"},
		{name: "asked for less", asked: setup.Limits{CPUs: 0.5, Memory: 512, Pids: 100}, given: "2:2048:4096"},
		{
			name: "asked for more of each", asked: setup.Limits{CPUs: 4, Memory: 5120, Pids: 8192}, given: "2:2048:4096",
			want: "4 CPUs (the box has 2), 5120 MiB of memory (the box has 2048), 8192 processes (the box has 4096)",
		},
		{name: "asked for more memory", asked: setup.Limits{CPUs: 2, Memory: 5120}, given: "2.5:2048:4096", want: "5120 MiB of memory (the box has 2048)"},
		{name: "a daemon that doesn't say", asked: setup.Limits{CPUs: 4}},
		{name: "a value that isn't the daemon's", asked: setup.Limits{CPUs: 4}, given: "lots"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shortOf(tt.asked, tt.given); got != tt.want {
				t.Errorf("shortOf(%+v, %q) = %q, want %q", tt.asked, tt.given, got, tt.want)
			}
		})
	}
}
