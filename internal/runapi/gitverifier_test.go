package runapi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestGitBaseVerifierRequiresAllowedReachability(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", repository}, {"-C", repository, "config", "user.email", "test@example.com"}, {"-C", repository, "config", "user.name", "Test"}} {
		if output, err := execGit(args...); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "file"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", repository, "add", "file"}, {"-C", repository, "commit", "-m", "one"}} {
		if output, err := execGit(args...); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	headRaw, err := execGit("-C", repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head, err := task.ParseGitOID(strings.TrimSpace(headRaw))
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewGitBaseVerifier(repository, "/usr/bin/git", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), head); err != nil {
		t.Fatalf("reachable head: %v", err)
	}
	if err := verifier.Verify(context.Background(), testBase); err == nil {
		t.Fatal("missing object accepted")
	}
	if output, err := execGit("-C", repository, "tag", "-a", "annotated", "-m", "tag"); err != nil {
		t.Fatalf("annotated tag: %s: %v", output, err)
	}
	tagRaw, err := execGit("-C", repository, "rev-parse", "annotated")
	if err != nil {
		t.Fatal(err)
	}
	tagOID, err := task.ParseGitOID(strings.TrimSpace(tagRaw))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), tagOID); err == nil {
		t.Fatal("annotated tag object accepted as an exact commit")
	}
}

func TestGitBaseVerifierUsesPromisorSafeEnvironment(t *testing.T) {
	directory := t.TempDir()
	observed := filepath.Join(directory, "observed")
	git := filepath.Join(directory, "git")
	script := "#!/bin/sh\n" +
		"printf '%s' \"$GIT_NO_LAZY_FETCH\" > " + strconv.Quote(observed) + "\n" +
		"case \"$*\" in\n" +
		"  *'rev-parse --is-inside-work-tree'*) printf 'true\\n' ;;\n" +
		"  *'cat-file -t'*) printf 'commit\\n' ;;\n" +
		"  *for-each-ref*) exit 0 ;;\n" +
		"  *'merge-base --is-ancestor'*) exit 0 ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewGitBaseVerifier(directory, git, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), testBase); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(observed)
	if err != nil || string(value) != "1" {
		t.Fatalf("GIT_NO_LAZY_FETCH=%q, error=%v", value, err)
	}
}

func execGit(args ...string) (string, error) {
	command := exec.Command("/usr/bin/git", args...)
	value, err := command.CombinedOutput()
	return string(value), err
}
