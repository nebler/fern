package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

func TestLoadAgeIdentitiesRejectsUnsafeFiles(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(path, []byte("# operator identity\n"+identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if identities, err := loadAgeIdentities([]string{path}); err != nil || len(identities) != 1 {
		t.Fatalf("identities = %d, error = %v", len(identities), err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgeIdentities([]string{path}); !errors.Is(err, errUnsafeIdentityFile) {
		t.Fatalf("group-readable identity error = %v, want errUnsafeIdentityFile", err)
	}
	if _, err := loadAgeIdentities([]string{path + "-missing"}); !errors.Is(err, errUnsafeIdentityFile) {
		t.Fatalf("missing identity file error = %v", err)
	}
}

func TestParseAgeRecipients(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if recipients, err := parseAgeRecipients([]string{" " + identity.Recipient().String() + " "}); err != nil || len(recipients) != 1 {
		t.Fatalf("recipients = %d, error = %v", len(recipients), err)
	}
	for _, values := range [][]string{nil, {"not-a-recipient"}} {
		if _, err := parseAgeRecipients(values); err == nil {
			t.Fatalf("parseAgeRecipients(%q) succeeded", values)
		}
	}
}
