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
	if err := Write(path, []byte("data"), 0o600); err == nil {
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

func TestRenameNoReplaceAndIdentity(t *testing.T) {
	directory := t.TempDir()
	source, target := filepath.Join(directory, "source"), filepath.Join(directory, "target")
	for _, path := range []string{source, target} {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RenameNoReplace(source, target); err == nil {
		t.Fatal("RenameNoReplace replaced an existing target")
	}
	before, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(source, target); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(directory); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	beforeDevice, beforeInode, err := Identity(before)
	afterDevice, afterInode, afterErr := Identity(after)
	if err != nil || afterErr != nil || beforeDevice != afterDevice || beforeInode != afterInode {
		t.Fatalf("identity changed across rename: %v %v", err, afterErr)
	}
	if _, _, err := Identity(nil); err == nil {
		t.Fatal("Identity accepted nil info")
	}
}

func TestWriteExclusiveRefusesExistingTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")
	if err := WriteExclusive(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteExclusive(path, []byte("second"), 0o600); err == nil {
		t.Fatal("WriteExclusive replaced an existing file")
	}
	if data, err := Read(path, 64); err != nil || string(data) != "first" {
		t.Fatalf("existing file = %q, %v", data, err)
	}
}

func TestQuarantineRemove(t *testing.T) {
	t.Run("removes the proven tree without following symlinks", func(t *testing.T) {
		parent, outside := t.TempDir(), t.TempDir()
		target := filepath.Join(parent, "tree")
		sentinel := filepath.Join(outside, "retain")
		if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sentinel, []byte("retain"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(target, "nested", "outside")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		device, inode, err := Identity(info)
		if err != nil {
			t.Fatal(err)
		}
		if err := QuarantineRemove(target, filepath.Join(parent, "quarantine"), device, inode); err != nil {
			t.Fatal(err)
		}
		if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
			t.Fatalf("parent entries = %v, %v", entries, err)
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "retain" {
			t.Fatalf("removal followed a symlink: %q, %v", data, err)
		}
	})
	t.Run("keeps a replaced object quarantined", func(t *testing.T) {
		parent := t.TempDir()
		target, quarantine := filepath.Join(parent, "tree"), filepath.Join(parent, "quarantine")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		device, inode, err := Identity(info)
		if err != nil {
			t.Fatal(err)
		}
		if err := QuarantineRemove(target, quarantine, device, inode+1); !errors.Is(err, ErrChanged) {
			t.Fatalf("identity mismatch error = %v", err)
		}
		if _, err := os.Lstat(quarantine); err != nil {
			t.Fatalf("mismatched object was not left in quarantine: %v", err)
		}
		if err := QuarantineRemove(target, quarantine+"-2", device, inode); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing path error = %v", err)
		}
	})
}
