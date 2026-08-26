package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/credentialbundle"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/registry"
	fernruntime "github.com/nebler/fern/internal/runtime"
)

type fakeCredentialRuntime struct {
	state       fernruntime.State
	archive     []byte
	replacement []byte
	replaces    int
	onReplace   func()
	closed      bool
}

func (runtime *fakeCredentialRuntime) Status(context.Context, string) (fernruntime.Observation, error) {
	return fernruntime.Observation{State: runtime.state}, nil
}
func (runtime *fakeCredentialRuntime) ExportWorkspaceGH(context.Context, fernruntime.Spec) ([]byte, error) {
	return append([]byte(nil), runtime.archive...), nil
}
func (runtime *fakeCredentialRuntime) ReplaceWorkspaceGH(_ context.Context, _ fernruntime.Spec, archive []byte, _ string) error {
	runtime.replaces++
	runtime.replacement = append([]byte(nil), archive...)
	if runtime.onReplace != nil {
		runtime.onReplace()
	}
	return nil
}
func (runtime *fakeCredentialRuntime) Close() error { runtime.closed = true; return nil }

func TestWorkspaceGHCredentialExportImportAndRotation(t *testing.T) {
	root := privateTestDirectory(t)
	configPath, envPath, stateDirectory, _ := backupFixture(t, root)
	identity := credentialTestIdentity(t, root)
	runtime := &fakeCredentialRuntime{state: fernruntime.StateAbsent, archive: credentialHostsArchive(t, "old-workspace-token-value")}
	restore := installCredentialTestHooks(t, runtime, func(_ context.Context, cfg config.Config, app *githubapp.AppCredentials, token string) error {
		if cfg.Workspace.GitHub.Repository.ID != 123 || app != nil || token != "old-workspace-token-value" {
			t.Fatalf("validation cfg=%+v app=%v token=%q", cfg.Workspace.GitHub, app, token)
		}
		return nil
	})
	defer restore()

	bundlePath := filepath.Join(root, "workspace-gh.age")
	common := []string{"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory}
	if err := runCredentialExport(append(common, "--generation", "generation-a", "--recipient", identity.Recipient().String(), "--output", bundlePath), testLogger()); err != nil {
		t.Fatal(err)
	}
	encrypted, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte("old-workspace-token-value")) {
		t.Fatal("export contains plaintext credential")
	}
	identityPath := writeCredentialIdentity(t, root, identity)
	rollbackPath := filepath.Join(root, "workspace-gh.rollback.age")
	runtime.onReplace = func() {
		if lease, err := registry.Acquire(filepath.Join(stateDirectory, "locks"), "demo"); err == nil {
			_ = lease.Release()
			t.Error("workspace lease was not held during replacement")
		}
	}
	if err := runCredentialImport(append(common, "--input", bundlePath, "--identity", identityPath, "--rollback-output", rollbackPath), testLogger(), false); err != nil {
		t.Fatal(err)
	}
	if runtime.replaces != 1 || !bytes.Equal(runtime.replacement, runtime.archive) {
		t.Fatal("workspace-gh candidate was not replaced")
	}
	if rollback, err := credentialbundle.ReadFile(rollbackPath, []age.Identity{identity}); err != nil || len(rollback.WorkspaceGH) == 0 {
		t.Fatalf("rollback artifact = %v, error = %v", rollback, err)
	}
	if err := runCredentialImport(append(common, "--input", bundlePath, "--identity", identityPath), testLogger(), true); err == nil || !strings.Contains(err.Error(), "acknowledge-external-revocation") {
		t.Fatalf("rotation acknowledgement error = %v", err)
	}
}

func TestCredentialImportRejectsCandidateBeforeMutationAndSanitizes(t *testing.T) {
	root := privateTestDirectory(t)
	configPath, envPath, stateDirectory, _ := backupFixture(t, root)
	identity := credentialTestIdentity(t, root)
	runtime := &fakeCredentialRuntime{state: fernruntime.StateAbsent, archive: credentialHostsArchive(t, "old-workspace-token-value")}
	const secret = "validator-secret-must-not-leak"
	restore := installCredentialTestHooks(t, runtime, func(context.Context, config.Config, *githubapp.AppCredentials, string) error {
		return errors.New(secret)
	})
	defer restore()
	bundle := credentialTestBundle(t, "generation-b", credentialHostsArchive(t, "candidate-workspace-token"))
	input := filepath.Join(root, "candidate.age")
	if err := credentialbundle.WriteFile(input, bundle, []age.Recipient{identity.Recipient()}); err != nil {
		t.Fatal(err)
	}
	rollback := filepath.Join(root, "must-not-exist.age")
	err := runCredentialImport([]string{"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory, "--input", input, "--identity", writeCredentialIdentity(t, root, identity), "--rollback-output", rollback}, testLogger(), false)
	if err == nil || strings.Contains(err.Error(), secret) || runtime.replaces != 0 || pathExists(rollback) {
		t.Fatalf("error=%v replaces=%d rollback=%t", err, runtime.replaces, pathExists(rollback))
	}
}

func TestCredentialOperationRefusesLiveLeaseAndCompute(t *testing.T) {
	root := privateTestDirectory(t)
	configPath, envPath, stateDirectory, _ := backupFixture(t, root)
	identity := credentialTestIdentity(t, root)
	args := []string{"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory, "--recipient", identity.Recipient().String(), "--output", filepath.Join(root, "out.age")}
	lease, err := registry.Acquire(filepath.Join(stateDirectory, "locks"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := runCredentialExport(args, testLogger()); err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("lease error = %v", err)
	}
	_ = lease.Release()
	runtime := &fakeCredentialRuntime{state: fernruntime.StateRunning}
	restore := installCredentialTestHooks(t, runtime, nil)
	defer restore()
	if err := runCredentialExport(args, testLogger()); err == nil || !strings.Contains(err.Error(), "absent compute") {
		t.Fatalf("compute error = %v", err)
	}
}

func TestGitHubAppCredentialExportAndImport(t *testing.T) {
	root := privateTestDirectory(t)
	configPath, envPath, stateDirectory, repository := appCredentialFixture(t, root)
	store, err := githubapp.NewCredentialStore(filepath.Join(stateDirectory, "github-app"))
	if err != nil {
		t.Fatal(err)
	}
	old := credentialTestApp(t, 741, "old-client-secret-value")
	if err := store.Save(old); err != nil {
		t.Fatal(err)
	}
	identity := credentialTestIdentity(t, root)
	runtime := &fakeCredentialRuntime{state: fernruntime.StateAbsent}
	validated := 0
	restore := installCredentialTestHooks(t, runtime, func(_ context.Context, cfg config.Config, app *githubapp.AppCredentials, token string) error {
		validated++
		if app == nil || app.AppID() != 741 || token != "" || cfg.Workspace.Repo != repository {
			t.Fatal("invalid app candidate validation")
		}
		return nil
	})
	defer restore()
	common := []string{"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory}
	exported := filepath.Join(root, "app-export.age")
	if err := runCredentialExport(append(common, "--generation", "app-old", "--recipient", identity.Recipient().String(), "--output", exported), testLogger()); err != nil {
		t.Fatal(err)
	}
	newCredentials := credentialTestApp(t, 741, "new-client-secret-value")
	payload, err := githubapp.MarshalStoredCredentials(newCredentials)
	if err != nil {
		t.Fatal(err)
	}
	bundle := credentialTestBundle(t, "app-new", nil)
	bundle.Binding.Mode = string(config.GitHubModeGitHubAppBroker)
	bundle.Binding.AppID = 741
	bundle.Binding.InstallationID = 91
	bundle.GitHubApp = payload
	input := filepath.Join(root, "app-candidate.age")
	if err := credentialbundle.WriteFile(input, bundle, []age.Recipient{identity.Recipient()}); err != nil {
		t.Fatal(err)
	}
	rollback := filepath.Join(root, "app-rollback.age")
	if err := runCredentialImport(append(common, "--input", input, "--identity", writeCredentialIdentity(t, root, identity), "--rollback-output", rollback), testLogger(), true); err == nil {
		t.Fatal("rotation without external revocation acknowledgement succeeded")
	}
	if err := runCredentialImport(append(common, "--input", input, "--identity", writeCredentialIdentity(t, root, identity), "--rollback-output", rollback, "--acknowledge-external-revocation"), testLogger(), true); err != nil {
		t.Fatal(err)
	}
	active, err := store.Load()
	if err != nil || active.ClientSecret() != "new-client-secret-value" || validated != 1 {
		t.Fatalf("active=%v validation=%d error=%v", active, validated, err)
	}
	rolledBack, err := credentialbundle.ReadFile(rollback, []age.Identity{identity})
	if err != nil {
		t.Fatal(err)
	}
	prior, err := githubapp.ParseStoredCredentials(rolledBack.GitHubApp)
	if err != nil || prior.ClientSecret() != "old-client-secret-value" {
		t.Fatal("encrypted rollback did not retain prior App generation")
	}
}

func installCredentialTestHooks(t *testing.T, runtime credentialDocker, validator credentialCandidateValidator) func() {
	t.Helper()
	oldDocker, oldValidator := openCredentialDocker, validateCredentialCandidates
	openCredentialDocker = func(*slog.Logger) (credentialDocker, error) { return runtime, nil }
	if validator != nil {
		validateCredentialCandidates = validator
	}
	return func() {
		openCredentialDocker, validateCredentialCandidates = oldDocker, oldValidator
	}
}

func credentialTestIdentity(t *testing.T, _ string) *age.X25519Identity {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func writeCredentialIdentity(t *testing.T, root string, identity *age.X25519Identity) string {
	t.Helper()
	path := filepath.Join(root, "identity.txt")
	if err := os.WriteFile(path, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func credentialHostsArchive(t *testing.T, token string) []byte {
	t.Helper()
	value := "github.com:\n  user: operator\n  users:\n    operator:\n      oauth_token: " + token + "\n"
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := writer.WriteHeader(&tar.Header{Name: "hosts.yml", Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(value))}); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(writer, value)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func credentialTestBundle(t *testing.T, generation string, archive []byte) credentialbundle.Bundle {
	t.Helper()
	return credentialbundle.Bundle{
		Version: credentialbundle.Version, Epoch: generation, CreatedAt: time.Now().UTC(),
		Binding:     credentialbundle.Binding{Workspace: "demo", Mode: string(config.GitHubModeWorkspaceGH), Hostname: "github.com", RepositoryID: 123, Repository: "owner/repository"},
		WorkspaceGH: archive,
	}
}

func credentialTestApp(t *testing.T, appID int64, clientSecret string) githubapp.AppCredentials {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	payload, err := json.Marshal(map[string]any{
		"version": 1, "app_id": appID, "client_id": "Iv1.client", "client_secret": clientSecret,
		"webhook_secret": "webhook-secret-value", "private_key_pem": string(keyPEM),
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := githubapp.ParseStoredCredentials(payload)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func appCredentialFixture(t *testing.T, root string) (configPath, envPath, stateDirectory, repository string) {
	t.Helper()
	repository = filepath.Join(root, "repository")
	stateDirectory = filepath.Join(root, "state")
	configDirectory := filepath.Join(root, "config")
	for _, directory := range []string{repository, stateDirectory, configDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath, envPath = filepath.Join(configDirectory, "fern.yaml"), filepath.Join(configDirectory, "fern.env")
	configuration := "workspace:\n  name: demo\n  image: image:test\n  repo: " + repository + "\n  memory: 1Gi\n  env:\n    OPENCODE_PASSWORD: ${OPENCODE_PASSWORD}\n  github:\n    mode: github-app-broker\n    hostname: github.com\n    installationId: 91\n    repository:\n      id: 123\n      fullName: owner/repository\ncontrol:\n  password: ${FERN_CONTROL_PASSWORD}\nproxy:\n  listen: 127.0.0.1:8080\n  operatorListen: 127.0.0.1:8081\n  remoteOrigin: https://fern.example.ts.net\n"
	writeFixtureFile(t, configPath, configuration)
	writeFixtureFile(t, envPath, "OPENCODE_PASSWORD=opencode-password-opencode-password\nFERN_CONTROL_PASSWORD=control-password-control-password\n")
	return configPath, envPath, stateDirectory, repository
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
