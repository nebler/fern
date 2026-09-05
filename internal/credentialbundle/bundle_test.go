package credentialbundle

import (
	"bytes"
	"encoding/json"
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
	if bytes.Contains(encrypted.Bytes(), bundle.GitHubApp) {
		t.Fatal("encrypted artifact contains plaintext credentials")
	}
	decoded, err := Decrypt(bytes.NewReader(encrypted.Bytes()), []age.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Epoch != bundle.Epoch || !bytes.Equal(decoded.GitHubApp, bundle.GitHubApp) {
		t.Fatal("bundle did not round trip")
	}
	secret := "app-secret-must-not-leak"
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
	bundle.Version = Version + 1
	var encrypted bytes.Buffer
	if err := Encrypt(&encrypted, bundle, []age.Recipient{identity.Recipient()}); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("invalid bundle error = %v", err)
	}
	formatted := fmt.Sprintf("%v %#v", testBundle(), testBundle())
	if strings.Contains(formatted, "app-secret-must-not-leak") {
		t.Fatalf("formatted bundle leaked secrets: %s", formatted)
	}
}

func testBundle() Bundle {
	return Bundle{
		Version: Version, Epoch: "generation-a", CreatedAt: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		Binding:   Binding{Workspace: "demo", Mode: "github-app-broker", Hostname: "github.com", AppID: 42, InstallationID: 7, RepositoryID: 123, Repository: "owner/repository"},
		GitHubApp: []byte(`{"version":1,"secret":"app-secret-must-not-leak"}`),
	}
}

func TestDecryptRejectsDuplicateRemovedAndLegacyFields(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(testBundle())
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"duplicate":        strings.Replace(string(payload), `"version":2`, `"version":2,"VERSION":2`, 1),
		"nested duplicate": strings.Replace(string(payload), `"secret":`, `"SECRET":"hidden","secret":`, 1),
		"removed field":    strings.TrimSuffix(string(payload), "}") + `,"workspace_gh":"secret-must-not-leak"}`,
		"removed mode":     strings.Replace(string(payload), "github-app-broker", "workspace-gh", 1),
		"legacy":           strings.Replace(string(payload), `"version":2`, `"version":1`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			var encrypted bytes.Buffer
			writer, err := age.Encrypt(&encrypted, identity.Recipient())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = Decrypt(&encrypted, []age.Identity{identity})
			if !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "must-not-leak") {
				t.Fatal("error disclosed credentials")
			}
			if name == "legacy" && !errors.Is(err, ErrLegacyBundle) {
				t.Fatalf("legacy error = %v", err)
			}
		})
	}
}

type pausedRecipient struct {
	age.Recipient
	ready  chan<- struct{}
	resume <-chan struct{}
}

func (recipient pausedRecipient) Wrap(key []byte) ([]*age.Stanza, error) {
	recipient.ready <- struct{}{}
	<-recipient.resume
	return recipient.Recipient.Wrap(key)
}

func TestConcurrentDestinationCreatorCannotBeClobbered(t *testing.T) {
	t.Parallel()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, creator := range []string{"file", "symlink", "bundle"} {
		t.Run(creator, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "credentials.age")
			ready, resume := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- WriteFile(path, testBundle(), []age.Recipient{pausedRecipient{identity.Recipient(), ready, resume}})
			}()
			<-ready
			var createErr error
			switch creator {
			case "file":
				createErr = os.WriteFile(path, []byte("existing"), 0o600)
			case "symlink":
				createErr = os.Symlink("absent-target", path)
			case "bundle":
				createErr = WriteFile(path, testBundle(), []age.Recipient{identity.Recipient()})
			}
			before, statErr := os.Lstat(path)
			close(resume)
			writeErr := <-done
			if createErr != nil || statErr != nil {
				t.Fatalf("create: %v; stat: %v", createErr, statErr)
			}
			if !errors.Is(writeErr, ErrUnsafeFile) {
				t.Fatalf("write error = %v", writeErr)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("destination replaced")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary artifact retained: %v, %v", entries, err)
			}
		})
	}
}
