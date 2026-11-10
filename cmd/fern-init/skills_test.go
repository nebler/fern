package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A Cloudflare repository's Claude gets the image's skills as links; another
// repository's loses them; someone's own skill is never touched.
func TestLinkSkills(t *testing.T) {
	from := t.TempDir()
	for _, s := range []string{"wrangler", "fern-deploy"} {
		if err := os.MkdirAll(filepath.Join(from, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(from, "LICENSE"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	claude := t.TempDir()
	mine := filepath.Join(claude, "skills", "mine")
	if err := os.MkdirAll(mine, 0o755); err != nil {
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

	for range 2 { // a second start finds them linked
		if err := linkSkills(claude, from, true); err != nil {
			t.Fatalf("linkSkills(on) error: %v", err)
		}
	}
	if got, want := skills(), []string{"fern-deploy", "mine", "wrangler"}; !slices.Equal(got, want) {
		t.Errorf("skills = %v, want %v", got, want)
	}
	if target, err := os.Readlink(filepath.Join(claude, "skills", "wrangler")); err != nil || target != filepath.Join(from, "wrangler") {
		t.Errorf("the wrangler link = %q, %v; want it to the image's", target, err)
	}

	// The image drops a skill: its link goes.
	if err := os.RemoveAll(filepath.Join(from, "wrangler")); err != nil {
		t.Fatal(err)
	}
	if err := linkSkills(claude, from, true); err != nil {
		t.Fatal(err)
	}
	if got, want := skills(), []string{"fern-deploy", "mine"}; !slices.Equal(got, want) {
		t.Errorf("skills after the image dropped one = %v, want %v", got, want)
	}

	// Not a Cloudflare repository any more: Fern's links go, someone's stay.
	if err := linkSkills(claude, from, false); err != nil {
		t.Fatal(err)
	}
	if got, want := skills(), []string{"mine"}; !slices.Equal(got, want) {
		t.Errorf("skills when off = %v, want %v", got, want)
	}
}
