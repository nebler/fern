package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/registry"
	fernruntime "github.com/nebler/fern/internal/runtime"
)

type fakeBackupDocker struct {
	destroyed string
	restored  map[string]string
}

func (docker *fakeBackupDocker) Destroy(_ context.Context, name string) error {
	docker.destroyed = name
	return nil
}

func (docker *fakeBackupDocker) ExportManagedVolumes(_ context.Context, spec fernruntime.Spec, destination string) ([]string, error) {
	names := fernruntime.ManagedVolumeNames(spec)
	for _, name := range names {
		directory := filepath.Join(destination, name)
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, err
		}
		filename, value := "session.db", "durable-session\n"
		if strings.HasSuffix(name, "-v1-gh-config") {
			filename, value = "hosts.yml", "oauth_token: workspace-secret\n"
		}
		if err := os.WriteFile(filepath.Join(directory, filename), []byte(value), 0o600); err != nil {
			return nil, err
		}
	}
	return names, nil
}

func (docker *fakeBackupDocker) RestoreManagedVolumes(_ context.Context, _ fernruntime.Spec, sources map[string]string, _ string) error {
	docker.restored = sources
	for name, source := range sources {
		if info, err := os.Stat(source); err != nil || !info.IsDir() {
			return fmt.Errorf("volume %s was not staged: %v", name, err)
		}
	}
	return nil
}

func (*fakeBackupDocker) Close() error { return nil }

func TestBackupCreateAndRestoreOperationalPaths(t *testing.T) {
	root := privateTestDirectory(t)
	configPath, envPath, stateDirectory, repository := backupFixture(t, root)
	backup := filepath.Join(root, "generation-a")
	docker := &fakeBackupDocker{}
	originalFactory := openBackupDocker
	openBackupDocker = func(*slog.Logger) (backupDocker, error) { return docker, nil }
	t.Cleanup(func() { openBackupDocker = originalFactory })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := runBackupCreate([]string{
		"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory,
		"--generation", "generation-a", "--output", backup,
	}, log); err != nil {
		t.Fatal(err)
	}
	if docker.destroyed != "demo" {
		t.Fatalf("destroyed workspace = %q", docker.destroyed)
	}
	manifestData, err := os.ReadFile(filepath.Join(backup, "BACKUP-MANIFEST.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		NamedVolumes []string `json:"named_volumes"`
		Credentials  struct {
			WorkspaceGH string `json:"workspace_gh"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.NamedVolumes) != 2 || manifest.Credentials.WorkspaceGH != "included-in-external-recipient" {
		t.Fatalf("backup manifest = %+v", manifest)
	}
	tampered := filepath.Join(root, "tampered")
	if err := copyPath(backup, tampered); err != nil {
		t.Fatal(err)
	}
	stateArchive, err := os.OpenFile(filepath.Join(tampered, "state.tar"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateArchive.WriteString("tamper"); err != nil {
		t.Fatal(err)
	}
	if err := stateArchive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runBackupRestore([]string{
		"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory,
		"--backup", tampered, "--credential-input", backup + ".credentials.tar",
		"--recovery-dir", filepath.Join(stateDirectory, "tamper-recovery"),
	}, log); err == nil {
		t.Fatal("restore accepted a checksum-tampered backup")
	}
	if docker.restored != nil {
		t.Fatal("tampered backup reached Docker volume activation")
	}
	excluded := filepath.Join(root, "excluded")
	if err := copyPath(backup, excluded); err != nil {
		t.Fatal(err)
	}
	rewriteBackupAsCredentialExcluded(t, excluded)
	err = runBackupRestore([]string{
		"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory,
		"--backup", excluded, "--recovery-dir", filepath.Join(root, "excluded-recovery"),
	}, log)
	if err == nil || !strings.Contains(err.Error(), "was not staged") || strings.Contains(err.Error(), "host backup archive utility") {
		t.Fatalf("excluded restore did not reach volume policy without an unauthorized credential input: %v", err)
	}

	writeFixtureFile(t, filepath.Join(repository, "work.txt"), "changed\n")
	writeFixtureFile(t, filepath.Join(stateDirectory, "tasks", "note"), "changed-state\n")
	if err := runBackupRestore([]string{
		"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory,
		"--backup", backup, "--recovery-dir", filepath.Join(stateDirectory, "recovery"),
	}, log); err != nil {
		t.Fatal(err)
	}
	if got := readFixtureFile(t, filepath.Join(repository, "work.txt")); got != "original\n" {
		t.Fatalf("repository after restore = %q", got)
	}
	if got := readFixtureFile(t, filepath.Join(stateDirectory, "tasks", "note")); got != "original-state\n" {
		t.Fatalf("state after restore = %q", got)
	}
	if len(docker.restored) != 2 {
		t.Fatalf("restored volume sources = %q", docker.restored)
	}
	rollbackRoot := filepath.Join(stateDirectory, "recovery", operationalRollbackDirectory)
	if !pathExists(filepath.Join(rollbackRoot, "ROLLBACK-MANIFEST.json")) || !pathExists(filepath.Join(rollbackRoot, "volumes", "fern-demo-v2-data")) {
		t.Fatal("restore did not retain durable filesystem and Docker rollback material")
	}
	if err := runBackupRollback([]string{
		"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory,
		"--recovery-dir", filepath.Join(stateDirectory, "recovery"),
	}, log); err != nil {
		t.Fatal(err)
	}
	if got := readFixtureFile(t, filepath.Join(repository, "work.txt")); got != "changed\n" {
		t.Fatalf("repository after durable rollback = %q", got)
	}
	if got := readFixtureFile(t, filepath.Join(stateDirectory, "tasks", "note")); got != "changed-state\n" {
		t.Fatalf("state after durable rollback = %q", got)
	}
	if !pathExists(filepath.Join(rollbackRoot, "ROLLBACK-MANIFEST.json")) || len(docker.restored) != 2 {
		t.Fatal("explicit rollback did not retain its restart-safe generation or restore Docker volumes")
	}
}

func TestBackupCreateRefusesLiveWorkspaceLease(t *testing.T) {
	root := privateTestDirectory(t)
	configPath, envPath, stateDirectory, _ := backupFixture(t, root)
	lease, err := registry.Acquire(filepath.Join(stateDirectory, "locks"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	err = runBackupCreate([]string{
		"--config", configPath, "--env-file", envPath, "--state-dir", stateDirectory,
		"--generation", "locked", "--output", filepath.Join(root, "backup"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("live lease error = %v", err)
	}
}

func TestCheckpointSQLiteTreeRejectsCorruption(t *testing.T) {
	root := privateTestDirectory(t)
	path := filepath.Join(root, "tasks.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA journal_mode=WAL; CREATE TABLE durable(value TEXT); INSERT INTO durable VALUES ('ok')"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkpointSQLiteTree(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkpointSQLiteTree(context.Background(), root); err == nil {
		t.Fatal("corrupt SQLite state passed integrity verification")
	}
}

func TestFilesystemRestoreStagesBeforeActivationAndRollsBack(t *testing.T) {
	root := privateTestDirectory(t)
	current := filepath.Join(root, "current")
	state := filepath.Join(root, "state")
	repository := filepath.Join(root, "repository")
	configPath := filepath.Join(root, "etc", "fern.yaml")
	envPath := filepath.Join(root, "etc", "fern.env")
	for _, directory := range []string{filepath.Join(current, "config"), filepath.Join(current, "state", "tasks"), filepath.Join(current, "repository"), filepath.Dir(configPath), filepath.Join(state, "tasks"), repository} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{
		filepath.Join(current, "config", "fern.yaml"): "new-config", filepath.Join(current, "config", "fern.env"): "new-env",
		filepath.Join(current, "state", "tasks", "db"): "new-state", filepath.Join(current, "repository", "work"): "new-repo",
		configPath: "old-config", envPath: "old-env", filepath.Join(state, "tasks", "db"): "old-state", filepath.Join(repository, "work"): "old-repo",
	} {
		writeFixtureFile(t, path, value)
	}
	transaction, err := prepareFilesystemRestore(current, state, configPath, envPath, repository, "generation-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFixtureFile(t, filepath.Join(repository, "work")); got != "old-repo" {
		t.Fatalf("prepare mutated repository: %q", got)
	}
	if err := transaction.Activate(); err != nil {
		t.Fatal(err)
	}
	if got := readFixtureFile(t, filepath.Join(repository, "work")); got != "new-repo" {
		t.Fatalf("activation repository = %q", got)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := readFixtureFile(t, filepath.Join(repository, "work")); got != "old-repo" {
		t.Fatalf("rollback repository = %q", got)
	}
}

func TestEmbeddedBackupArchiveToolIsRunnable(t *testing.T) {
	var output bytes.Buffer
	if err := runBackupArchiveTool(context.Background(), "", []string{"--help"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Deterministic, fail-closed host backup") {
		t.Fatalf("embedded utility help = %q", output.String())
	}
}

func TestRestoreCredentialInputFollowsBackupPolicy(t *testing.T) {
	root := privateTestDirectory(t)
	for _, test := range []struct {
		name, policy, requested, want string
	}{
		{name: "external default", policy: "external"},
		{name: "external explicit", policy: "external", requested: filepath.Join(root, "recipient.tar"), want: filepath.Join(root, "recipient.tar")},
		{name: "exclude", policy: "exclude"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := filepath.Join(root, strings.ReplaceAll(test.name, " ", "-"))
			if test.name == "external default" {
				test.want = bundle + ".credentials.tar"
			}
			if err := os.Mkdir(bundle, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := fmt.Sprintf(`{"generation":"generation-a","named_volumes":[],"credentials":{"policy":%q}}`, test.policy)
			writeFixtureFile(t, filepath.Join(bundle, "BACKUP-MANIFEST.json"), manifest)
			got, err := credentialInputForRestore(bundle, test.requested)
			if err != nil || got != test.want {
				t.Fatalf("credential input = %q, error = %v, want %q", got, err, test.want)
			}
		})
	}
}

func backupFixture(t *testing.T, root string) (configPath, envPath, stateDirectory, repository string) {
	t.Helper()
	repository = filepath.Join(root, "repository")
	stateDirectory = filepath.Join(root, "state")
	configDirectory := filepath.Join(root, "config")
	for _, directory := range []string{repository, filepath.Join(stateDirectory, "tasks"), configDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath = filepath.Join(configDirectory, "fern.yaml")
	envPath = filepath.Join(configDirectory, "fern.env")
	configuration := "workspace:\n  name: demo\n  image: image:test\n  repo: " + repository + "\n  memory: 1Gi\n  env:\n    OPENCODE_PASSWORD: ${OPENCODE_PASSWORD}\n  github:\n    mode: workspace-gh\n    hostname: github.com\n    repository:\n      id: 123\n      fullName: owner/repository\ncontrol:\n  password: ${FERN_CONTROL_PASSWORD}\nproxy:\n  listen: 127.0.0.1:8080\n  operatorListen: 127.0.0.1:8081\n  remoteOrigin: https://fern.example.ts.net\n"
	writeFixtureFile(t, configPath, configuration)
	writeFixtureFile(t, envPath, "OPENCODE_PASSWORD=opencode-password-opencode-password\nFERN_CONTROL_PASSWORD=control-password-control-password\n")
	writeFixtureFile(t, filepath.Join(repository, "work.txt"), "original\n")
	writeFixtureFile(t, filepath.Join(stateDirectory, "tasks", "note"), "original-state\n")
	return configPath, envPath, stateDirectory, repository
}

func privateTestDirectory(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFixtureFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func rewriteBackupAsCredentialExcluded(t *testing.T, bundle string) {
	t.Helper()
	manifestPath := filepath.Join(bundle, "BACKUP-MANIFEST.json")
	var manifest map[string]any
	data, err := os.ReadFile(manifestPath)
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatal(err)
	}
	credentials := manifest["credentials"].(map[string]any)
	credentials["policy"] = "exclude"
	credentials["workspace_gh"] = "excluded-reauthorize"
	credentials["external"] = nil
	data, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	sumsPath := filepath.Join(bundle, "SHA256SUMS")
	sums := strings.Split(readFixtureFile(t, sumsPath), "\n")
	for index, line := range sums {
		if strings.HasSuffix(line, "  BACKUP-MANIFEST.json") {
			sums[index] = fmt.Sprintf("%x  BACKUP-MANIFEST.json", sum)
		}
	}
	if err := os.WriteFile(sumsPath, []byte(strings.Join(sums, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}
