package credentialbundle

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func TestBundleEncryptionRoundTripWrongIdentityAndTamper(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	bundle := testBundle()
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bundle, []age.Recipient{identity.Recipient()}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted.Bytes(), bundle.WorkspaceGH) || bytes.Contains(encrypted.Bytes(), bundle.GitHubApp) {
		t.Fatal("encrypted artifact contains plaintext credentials")
	}
	decoded, err := Decrypt(bytes.NewReader(encrypted.Bytes()), []age.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Epoch != bundle.Epoch || !bytes.Equal(decoded.WorkspaceGH, bundle.WorkspaceGH) || !bytes.Equal(decoded.GitHubApp, bundle.GitHubApp) {
		t.Fatal("bundle did not round trip")
	}
	secret := string(bundle.WorkspaceGH)
	if _, err := Decrypt(bytes.NewReader(encrypted.Bytes()), []age.Identity{wrong}); !errors.Is(err, ErrDecryptBundle) || strings.Contains(err.Error(), secret) {
		t.Fatalf("wrong identity error = %v", err)
	}
	tampered := append([]byte(nil), encrypted.Bytes()...)
	tampered[len(tampered)-1] ^= 1
	if _, err := Decrypt(bytes.NewReader(tampered), []age.Identity{identity}); !errors.Is(err, ErrDecryptBundle) || strings.Contains(err.Error(), secret) {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestBundleFilesAreEncryptedPrivateAndExclusive(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "credentials.age")
	if err := WriteFile(path, testBundle(), []age.Recipient{identity.Recipient()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("artifact info = %v, %v", info, err)
	}
	if err := WriteFile(path, testBundle(), []age.Recipient{identity.Recipient()}); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("replacement error = %v", err)
	}
	decoded, err := ReadFile(path, []age.Identity{identity})
	if err != nil || decoded.Binding.RepositoryID != 123 {
		t.Fatalf("decoded = %v, error = %v", decoded, err)
	}
}

func TestLoadIdentitiesRequiresPrivateValidFiles(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "identity.txt")
	if err := os.WriteFile(path, []byte("# operator identity\n"+identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if identities, err := LoadIdentities([]string{path}); err != nil || len(identities) != 1 {
		t.Fatalf("identities = %d, error = %v", len(identities), err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentities([]string{path}); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("public identity file error = %v", err)
	}
}

func TestBundleStrictCandidateRejectionAndRedaction(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	bundle := testBundle()
	bundle.Version = 2
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bundle, []age.Recipient{identity.Recipient()}); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("invalid bundle error = %v", err)
	}
	formatted := fmt.Sprintf("%v %#v", testBundle(), testBundle())
	if strings.Contains(formatted, "workspace-token-must-not-leak") || strings.Contains(formatted, "app-secret-must-not-leak") {
		t.Fatalf("formatted bundle leaked secrets: %s", formatted)
	}
}

func testBundle() Bundle {
	return Bundle{
		Version: Version, Epoch: "generation-a", CreatedAt: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		Binding:   Binding{Workspace: "demo", Mode: "workspace-gh", Hostname: "github.com", AppID: 42, RepositoryID: 123, Repository: "owner/repository"},
		GitHubApp: []byte(`{"secret":"app-secret-must-not-leak"}`), WorkspaceGH: []byte("workspace-token-must-not-leak"),
	}
}
