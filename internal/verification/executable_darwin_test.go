//go:build darwin

package verification

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDarwinMaterializationIgnoresTMPDIRAndCleansUp(t *testing.T) {
	git := darwinRealGit(t)
	identity, err := inspectExecutable(git)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "untrusted"))
	executable, err := preparePinnedExecutable(git, identity)
	if err != nil {
		t.Fatal(err)
	}
	path := executable.path
	if !strings.HasPrefix(path, darwinMaterializationRoot+string(filepath.Separator)) {
		executable.close()
		t.Fatalf("materialized path = %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		executable.close()
		t.Fatal(err)
	}
	executable.close()
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private materialization was not removed: %v", err)
	}
}

func TestDarwinAppleGitShimFailsConstructionRatherThanLaunchingByPath(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip(err)
	}
	_, err := NewRunner(RunnerConfig{GitExecutable: "/usr/bin/git", GitTimeout: 5 * time.Second,
		Name: "git", Version: "v1", ImageDigest: "sha256:git"})
	if !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner(/usr/bin/git) error = %v", err)
	}
}

func darwinRealGit(t *testing.T) string {
	t.Helper()
	for _, path := range []string{"/Library/Developer/CommandLineTools/usr/bin/git", "/Applications/Xcode.app/Contents/Developer/usr/bin/git"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	t.Skip("real Apple Git executable is unavailable")
	return ""
}
