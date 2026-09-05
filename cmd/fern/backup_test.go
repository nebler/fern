package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupLoadsOnlyCurrentConfiguration(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	envPath := filepath.Join(directory, "fern.env")
	data, err := os.ReadFile("../../fern.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	current := strings.Replace(string(data), "/srv/fern/repository", directory, 1)
	current = strings.Replace(current, "sha256:REPLACE_WITH_QUALIFIED_LOCAL_IMAGE_ID", "sha256:"+strings.Repeat("b", 64), 1)
	if err := os.WriteFile(envPath, []byte("FERN_CONTROL_PASSWORD="+strings.Repeat("s", 32)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	options := backupOptions{configPath: path, envPath: envPath}
	cfg, name, err := loadBackupConfig(options)
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo" || cfg.Workspace.Repo != directory || cfg.Workspace.GitHub.Repository.ID != 123456789 || cfg.Control.Password != strings.Repeat("s", 32) {
		t.Fatalf("backup configuration = %+v, %q", cfg, name)
	}
	for _, retired := range []string{"  image: retired\n", "  memory: 8Gi\n", "  env: {}\n"} {
		if err := os.WriteFile(path, []byte(strings.Replace(current, "workspace:\n", "workspace:\n"+retired, 1)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadBackupConfig(options); err == nil {
			t.Fatalf("backup accepted retired setting %q", retired)
		}
	}
}

func TestStageFernStateRetainsAuthorityAndDropsDisposableWork(t *testing.T) {
	source := filepath.Join(t.TempDir(), "state")
	destination := filepath.Join(t.TempDir(), "staged")
	for _, directory := range []string{
		"control", "github-app", "tasks/demo-background/artifact-cas/sha256:artifact",
		"tasks/demo-background/artifact-work", "tasks/demo-background/runtime",
		"tasks/demo-background/runtime/clone", "tasks/demo-publication", "locks", "recovery",
	} {
		if err := os.MkdirAll(filepath.Join(source, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(relative, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, relative), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("control/devices.json", "devices")
	write("github-app/app-credentials.json", "credentials")
	write("tasks/task-store.sqlite", "store")
	write("tasks/demo-background/artifact-cas/sha256:artifact/manifest.json", "manifest")
	write("tasks/demo-background/artifact-work/scratch", "scratch")
	write("tasks/demo-background/runtime/host.key", "host-key")
	write("tasks/demo-background/runtime/clone/repository", "clone")
	write("tasks/demo-publication/checkout", "publication")
	write("locks/operator.lock", "lock")
	write("recovery/generation", "recovery")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := stageFernState(source, destination); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"control/devices.json", "github-app/app-credentials.json", "tasks/task-store.sqlite",
		"tasks/demo-background/artifact-cas/sha256:artifact/manifest.json", "tasks/demo-background/runtime/host.key",
	} {
		if _, err := os.Stat(filepath.Join(destination, relative)); err != nil {
			t.Fatalf("durable authority %s was not staged: %v", relative, err)
		}
	}
	for _, relative := range []string{
		"tasks/demo-background/artifact-work", "tasks/demo-background/runtime/clone",
		"tasks/demo-publication", "locks", "recovery",
	} {
		if _, err := os.Stat(filepath.Join(destination, relative)); !os.IsNotExist(err) {
			t.Fatalf("disposable state %s was staged: %v", relative, err)
		}
	}
}
