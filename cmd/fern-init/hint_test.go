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
	if err := writeHint(dir, map[string]service{"redis": {}, "postgres": {}}, "http://{port}-demo.localhost:30780/"); err != nil {
		t.Fatalf("writeHint() error: %v", err)
	}
	if got := read(); !strings.Contains(got, "postgres, redis") || !strings.HasPrefix(got, hintMark) ||
		!strings.Contains(got, "http://5173-demo.localhost:30780/") {
		t.Errorf("hint = %q, want it marked, naming postgres, redis and a preview's link", got)
	}
	if err := writeHint(dir, nil, ""); err != nil {
		t.Fatalf("writeHint() again error: %v", err)
	}
	if got := read(); strings.Contains(got, "postgres") {
		t.Errorf("hint after the services went = %q, want it replaced", got)
	}
	if err := os.WriteFile(path, []byte("my own notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeHint(dir, nil, ""); err != nil {
		t.Fatalf("writeHint() over someone's file error: %v", err)
	}
	if got := read(); got != "my own notes\n" {
		t.Errorf("CLAUDE.md = %q after writeHint, want someone's own notes kept", got)
	}
}
