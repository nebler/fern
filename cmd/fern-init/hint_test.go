package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if got := read(); strings.Contains(got, "postgres") || strings.Contains(got, "Cloudflare") {
		t.Errorf("hint after the services went = %q, want it replaced, and nothing on Cloudflare", got)
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
