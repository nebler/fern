package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The node's packs, then those Fern detects, then the repository's, less
// those it turns off; names that can't be packs are dropped.
func TestActivePacks(t *testing.T) {
	got := activePacks("safety design", []string{"cloudflare"}, []string{"engineering", "../etc", "design"}, []string{"safety"})
	if want := []string{"design", "cloudflare", "engineering"}; !slices.Equal(got, want) {
		t.Errorf("activePacks() = %v, want %v", got, want)
	}
}

// Claude gets the active packs' skills as links, loses a pack's when it
// goes off or the image drops it, and keeps someone's own skill.
func TestLinkSkills(t *testing.T) {
	root := t.TempDir()
	for _, s := range []string{"cloudflare/wrangler", "cloudflare/fern-deploy", "design/animate", "design/wrangler"} {
		if err := os.MkdirAll(filepath.Join(root, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"cloudflare/LICENSE", "design/CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("Animate with care.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claude := t.TempDir()
	if err := os.MkdirAll(filepath.Join(claude, "skills", "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	skills := func() []string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(claude, "skills"))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	link := func(packs ...string) {
		t.Helper()
		if err := linkSkills(claude, root, packs); err != nil {
			t.Fatalf("linkSkills(%v) error: %v", packs, err)
		}
	}

	link("cloudflare", "design", "missing")
	link("cloudflare", "design", "missing") // a second start finds them linked
	if got, want := skills(), []string{"animate", "fern-deploy", "mine", "wrangler"}; !slices.Equal(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
	if target, _ := os.Readlink(filepath.Join(claude, "skills", "wrangler")); target != filepath.Join(root, "cloudflare", "wrangler") {
		t.Errorf("wrangler links to %q, want the first pack's", target)
	}
	if notes, err := packNotes(root, []string{"cloudflare", "design"}); err != nil || !slices.Equal(notes, []string{"Animate with care."}) {
		t.Errorf("packNotes() = %q, %v; want design's", notes, err)
	}

	// Cloudflare off: its links go, and design's wrangler takes the name.
	link("design")
	if got, want := skills(), []string{"animate", "mine", "wrangler"}; !slices.Equal(got, want) {
		t.Errorf("skills with design only = %v, want %v", got, want)
	}
	if target, _ := os.Readlink(filepath.Join(claude, "skills", "wrangler")); target != filepath.Join(root, "design", "wrangler") {
		t.Errorf("wrangler links to %q, want design's", target)
	}

	// Nothing on: only someone's own skill is left.
	link()
	if got, want := skills(), []string{"mine"}; !slices.Equal(got, want) {
		t.Errorf("skills with none = %v, want %v", got, want)
	}
}
