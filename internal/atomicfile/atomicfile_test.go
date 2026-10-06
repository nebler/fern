package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesAndReplaces(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	for _, content := range []string{"first", "second"} {
		if err := Write(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		data, err := Read(path, 64)
		if err != nil || string(data) != content {
			t.Fatalf("Read = %q, %v; want %q", data, err, content)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", info.Mode(), err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory entries = %v, %v; want only the target", entries, err)
	}
}

func TestWriteFailureLeavesTargetUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	if err := Write(path, []byte("data"), 0o600); err == nil || errors.Is(err, ErrNotDurable) {
		t.Fatalf("Write into missing directory = %v", err)
	}
}

func TestReadLimitsAndPassesThroughNotExist(t *testing.T) {
	directory := t.TempDir()
	if _, err := Read(filepath.Join(directory, "absent"), 8); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v", err)
	}
	path := filepath.Join(directory, "big")
	if err := os.WriteFile(path, []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, 8); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized file error = %v", err)
	}
	if data, err := Read(path, 9); err != nil || string(data) != "123456789" {
		t.Fatalf("exact-limit read = %q, %v", data, err)
	}
	if _, err := Read(directory, 8); err == nil {
		t.Fatal("Read accepted a directory")
	}
}

func TestPrivateDir(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a", "b")
	if err := PrivateDir(path); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("created mode = %v, %v", info.Mode(), err)
	}
	if err := os.Chmod(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDir(path); !errors.Is(err, ErrUnsafeDir) {
		t.Fatalf("group-accessible directory error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDir(link); !errors.Is(err, ErrUnsafeDir) {
		t.Fatalf("symlinked directory error = %v", err)
	}
}
