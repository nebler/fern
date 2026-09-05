package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/observability"
)

func TestLegacyPublicationQuarantineCommandRequiresLeaseAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	directory := filepath.Join(home, ".fern", "control")
	writeLegacyControlFixture(t, directory, "demo")

	lease, err := acquireHostLease("demo")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = runLegacyPublicationQuarantine([]string{"--name", "demo"}, &output)
	if err == nil || !strings.Contains(err.Error(), "fern up to be stopped") {
		t.Fatalf("contended quarantine error = %v", err)
	}
	if releaseErr := lease.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}

	if err := runLegacyPublicationQuarantine([]string{"--name", "demo"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "pending\trequested\tquarantined\t") {
		t.Fatalf("quarantine output = %q", output.String())
	}
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	pending, exists := store.Publication("pending")
	if !exists || pending.State != control.PublicationQuarantined || pending.OriginalState != control.PublicationRequested || pending.QuarantineReason != control.LegacyPublicationQuarantineReason || pending.QuarantinedAt.IsZero() {
		t.Fatalf("pending publication = %+v, exists=%t", pending, exists)
	}
	published, exists := store.Publication("published")
	if !exists || published.State != control.PublicationPublished || published.PullURL != "https://github.com/owner/repo/pull/1" || !published.QuarantinedAt.IsZero() {
		t.Fatalf("published publication changed = %+v, exists=%t", published, exists)
	}

	output.Reset()
	if err := runLegacyPublicationQuarantine([]string{"--name", "demo"}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "no unresolved legacy control publications\n" {
		t.Fatalf("idempotent output = %q", output.String())
	}
}

func TestUnquarantinedLegacyPublicationBlocksReadiness(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	writeLegacyControlFixture(t, directory, "demo")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	status := observability.NewRegistry()
	updateLegacyPublicationReadiness(status, store)
	if status.Snapshot().Ready {
		t.Fatal("unquarantined legacy publication left service ready")
	}
	if _, err := store.QuarantineLegacyPublications(time.Now()); err != nil {
		t.Fatal(err)
	}
	updateLegacyPublicationReadiness(status, store)
	if !status.Snapshot().Ready {
		t.Fatal("quarantined legacy publication left service unready")
	}
}

func writeLegacyControlFixture(t *testing.T, directory, workspace string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(workspace))
	path := filepath.Join(directory, fmt.Sprintf("%x.json", hash))
	data := `{"version":1,"workspace":"` + workspace + `","devices":{},"workflows":{},"publications":{"pending":{"id":"pending","workflowId":"workflow","state":"requested","operation":"operation","title":"Pending","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"},"published":{"id":"published","workflowId":"workflow","state":"published","operation":"operation","title":"Published","pullUrl":"https://github.com/owner/repo/pull/1","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
