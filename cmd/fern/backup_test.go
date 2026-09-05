package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
