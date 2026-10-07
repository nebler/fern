package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAcquireVerifiesOnceAndReturnsOwnedCheckout(t *testing.T) {
	e, repository, base := testEngineRepository(t)
	writeFile(t, filepath.Join(repository, "modified"), []byte("acquired\n"), 0o600)
	want, staged, err := e.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 42))
	if err != nil {
		t.Fatal(err)
	}
	locator, err := e.Store(context.Background(), staged)
	if err != nil {
		t.Fatal(err)
	}

	// Observe real Git invocations, not merely calls through an interface: each
	// full verification runs fsck, and each materialization detaches a checkout.
	log := filepath.Join(filepath.Dir(e.workRoot), "acquire-git.log")
	wrapper := filepath.Join(filepath.Dir(e.workRoot), "acquire-git")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(log) + "\nexec " + quote(e.gitExecutable) + " \"$@\"\n"
	writeFile(t, wrapper, []byte(script), 0o700)
	e.gitExecutable = wrapper
	e.gitFile, err = os.Stat(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, checkout, err := e.Acquire(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	defer checkout.Close()
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("acquired snapshot differs: got %+v want %+v", snapshot, want)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(commands), " fsck --strict --full"); count != 1 {
		t.Fatalf("full verification count=%d, want 1\n%s", count, commands)
	}
	if count := strings.Count(string(commands), " checkout --detach "); count != 1 {
		t.Fatalf("materialization count=%d, want 1\n%s", count, commands)
	}
	path := checkout.Path()
	if got, err := os.ReadFile(filepath.Join(path, "modified")); err != nil || string(got) != "acquired\n" {
		t.Fatalf("checkout content=%q: %v", got, err)
	}
	if err := checkout.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout remains after Close: %v", err)
	}

	// A previous acquisition is not an integrity cache.
	bundle := filepath.Join(e.casRoot, locator.digest.String(), bundleName)
	if err := os.Chmod(bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, bundle, []byte("corrupt"), 0o600)
	if err := os.Chmod(bundle, 0o400); err != nil {
		t.Fatal(err)
	}
	got, failedCheckout, err := e.Acquire(context.Background(), locator)
	if err == nil || failedCheckout != nil || !reflect.DeepEqual(got, Snapshot{}) {
		t.Fatalf("corrupt acquisition returned snapshot=%+v checkout=%v error=%v", got, failedCheckout, err)
	}
	if entries, err := os.ReadDir(e.workRoot); err != nil || len(entries) != 0 {
		t.Fatalf("failed acquisition leaked work: %v, %v", entries, err)
	}
}

func TestMaterializeVerifiedRejectsSourceChangeAndCleans(t *testing.T) {
	e, repository, base := testEngineRepository(t)
	_, staged, err := e.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 42))
	if err != nil {
		t.Fatal(err)
	}
	locator, err := e.Store(context.Background(), staged)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := e.Inspect(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a same-size CAS mutation between acquisition's full verification
	// and checkout copy. The private materialization step must rehash the copy.
	bundle := filepath.Join(e.casRoot, locator.digest.String(), bundleName)
	content, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	content[len(content)-1] ^= 0xff
	if err := os.Chmod(bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, bundle, content, 0o600)
	if err := os.Chmod(bundle, 0o400); err != nil {
		t.Fatal(err)
	}
	checkout, err := e.materializeVerified(context.Background(), locator, snapshot)
	if !errors.Is(err, ErrCheckout) || checkout != nil {
		t.Fatalf("changed source checkout=%v error=%v", checkout, err)
	}
	if entries, err := os.ReadDir(e.workRoot); err != nil || len(entries) != 0 {
		t.Fatalf("changed source leaked work: %v, %v", entries, err)
	}
	if len(e.checkouts) != 0 {
		t.Fatal("failed materialization retained checkout ownership")
	}
}
