package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestVersionCommandDevelopmentBuild(t *testing.T) {
	oldVersion, oldCommit := version, commit
	version, commit = "dev", "unknown"
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })

	var output bytes.Buffer
	if err := runVersion(nil, &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "fern dev (commit unknown)\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestVersionCommandInjectedValues(t *testing.T) {
	oldVersion, oldCommit := version, commit
	version, commit = "v1.2.3", "abc1234"
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })

	var output bytes.Buffer
	if err := runVersion(nil, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, version) || !strings.Contains(got, commit) {
		t.Fatalf("output %q does not contain injected version and commit", got)
	}
}

func TestRunVersionSucceeds(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run([]string{"version"}, log); err != nil {
		t.Fatal(err)
	}
}

func TestUsageIncludesVersion(t *testing.T) {
	if !strings.Contains(usageText, "version") {
		t.Fatalf("usage %q does not include version", usageText)
	}
}
