package verification_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/verification"
)

const helperEnvironment = "FERN_VERIFICATION_HELPER"

const (
	fakeGitExecutableEnvironment = "FERN_VERIFICATION_FAKE_GIT_EXECUTABLE"
	fakeGitMutationEnvironment   = "FERN_VERIFICATION_FAKE_GIT_MUTATION"
)

var immutableTestExecutable string

func TestMain(m *testing.M) {
	if isVerificationHelperArgs(os.Args) {
		os.Exit(m.Run())
	}
	if os.Getenv(fakeGitExecutableEnvironment) != "" {
		os.Exit(runFakeGit())
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stdout, "git version verification-test")
		os.Exit(0)
	}
	directory, err := os.MkdirTemp("", "fern-verification-test-")
	if err != nil {
		panic(err)
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		_ = os.RemoveAll(directory)
		panic(err)
	}
	source, err := os.Executable()
	if err == nil {
		source, err = filepath.EvalSymlinks(source)
	}
	immutableTestExecutable = filepath.Join(directory, "verification.test")
	if err == nil {
		err = copyNativeExecutable(source, immutableTestExecutable)
	}
	if err != nil {
		_ = os.RemoveAll(directory)
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(directory)
	os.Exit(code)
}

func isVerificationHelperArgs(arguments []string) bool {
	for index, argument := range arguments {
		if argument == "--" && index+1 < len(arguments) && arguments[index+1] == "fern-verification-helper" {
			return true
		}
	}
	return false
}

type gitFixture struct {
	git     string
	repo    string
	base    task.GitOID
	result  task.GitOID
	runner  *verification.Runner
	request verification.Request
}

func TestRunSuccessAndImmutablePolicy(t *testing.T) {
	fixture := newGitFixture(t)
	argv := helperArgv(t, "success")
	environment := map[string]string{helperEnvironment: "1"}
	policy := newPolicy(t, argv, environment, "", 5*time.Second, 1024)

	argv[4] = "failure"
	environment[helperEnvironment] = "changed"
	returnedArgv := policy.Argv()
	returnedArgv[3] = "failure"

	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Success || !result.Executed || result.Failure != verification.FailureNone {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.CheckName != "unit.check" || result.ExitCode != 0 || result.Signal != "" || result.TimedOut || result.Cancelled {
		t.Fatalf("unexpected execution metadata: %+v", result)
	}
	if result.StartedAt.IsZero() || result.EndedAt.Before(result.StartedAt) {
		t.Fatalf("invalid timestamps: %v - %v", result.StartedAt, result.EndedAt)
	}
	assertEvidence(t, result.Stdout, []byte("stdout-success\n"), 1024)
	assertEvidence(t, result.Stderr, []byte("stderr-success\n"), 1024)
}

func TestRunPinnedExecutable(t *testing.T) {
	fixture := newGitFixture(t)
	t.Run("native argv0", func(t *testing.T) {
		argv := helperArgv(t, "argv0")
		argv = append(argv, argv[0])
		result, err := fixture.runner.Run(context.Background(), newPolicy(t, argv, map[string]string{helperEnvironment: "1"}, "", 5*time.Second, 128), fixture.request)
		if err != nil || !result.Success {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "check")
	if err := copyNativeExecutable(immutableTestExecutable, executable); err != nil {
		t.Fatal(err)
	}
	policy := newPolicy(t, append([]string{executable}, helperArgv(t, "success")[1:]...),
		map[string]string{helperEnvironment: "1"}, "", 5*time.Second, 128)

	t.Run("replacement after NewPolicy", func(t *testing.T) {
		replacement := filepath.Join(directory, "replacement")
		if err := copyNativeExecutable(immutableTestExecutable, replacement); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, executable); err != nil {
			t.Fatal(err)
		}
		result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
		if !errors.Is(err, verification.ErrExecution) || result.Executed || result.Failure != verification.FailureStart {
			t.Fatalf("replacement executed: result=%+v err=%v", result, err)
		}
	})

	t.Run("same inode content mutation", func(t *testing.T) {
		executable := filepath.Join(directory, "same-inode")
		if err := copyNativeExecutable(immutableTestExecutable, executable); err != nil {
			t.Fatal(err)
		}
		policy := newPolicy(t, append([]string{executable}, helperArgv(t, "success")[1:]...),
			map[string]string{helperEnvironment: "1"}, "", 5*time.Second, 128)
		mutateExecutable(t, executable)
		result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
		if !errors.Is(err, verification.ErrExecution) || result.Executed || result.Failure != verification.FailureStart {
			t.Fatalf("mutated executable ran: result=%+v err=%v", result, err)
		}
	})
}

func TestNewPolicyRejectsScripts(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "check")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0500); err != nil {
		t.Fatal(err)
	}
	_, err = verification.NewPolicy(verification.PolicyConfig{CheckName: "check", Argv: []string{executable},
		Timeout: time.Second, OutputBytes: 64})
	if !errors.Is(err, verification.ErrInvalidPolicy) {
		t.Fatalf("script error = %v", err)
	}
}

func TestRunCommandFailure(t *testing.T) {
	fixture := newGitFixture(t)
	policy := helperPolicy(t, "failure", 5*time.Second, 1024)

	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if err != nil {
		t.Fatalf("command failure must be a recorded outcome, got %v", err)
	}
	if result.Success || result.Failure != verification.FailureCommand || result.ExitCode != 7 || !result.Executed {
		t.Fatalf("unexpected failure result: %+v", result)
	}
	assertEvidence(t, result.Stdout, []byte("failure-output\n"), 1024)
	assertEvidence(t, result.Stderr, []byte("failure-error\n"), 1024)
}

func TestRunTimeoutAndCancellation(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		fixture := newGitFixture(t)
		policy := helperPolicy(t, "sleep", 500*time.Millisecond, 1024)
		started := time.Now()
		result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if result.Success || !result.TimedOut || result.Cancelled || result.Failure != verification.FailureTimeout {
			t.Fatalf("unexpected timeout result: %+v", result)
		}
		if time.Since(started) > 4*time.Second {
			t.Fatal("timed-out process was not terminated promptly")
		}
		if !bytes.HasPrefix(result.Stdout.Prefix, []byte("sleeping\n")) {
			t.Fatalf("stdout was not drained: %q", result.Stdout.Prefix)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		fixture := newGitFixture(t)
		startedPath := filepath.Join(t.TempDir(), "started")
		policy := newPolicy(t, helperArgv(t, "sleep-signal", startedPath), map[string]string{helperEnvironment: "1"}, "", 10*time.Second, 1024)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for {
				if _, err := os.Stat(startedPath); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
		}()
		result, err := fixture.runner.Run(ctx, policy, fixture.request)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if result.Success || result.TimedOut || !result.Cancelled || result.Failure != verification.FailureCancelled {
			t.Fatalf("unexpected cancellation result: %+v", result)
		}
	})
}

func TestRunHugeOutputEvidence(t *testing.T) {
	fixture := newGitFixture(t)
	const size = 256*1024 + 37
	const limit = 97
	policy := newPolicy(t, helperArgv(t, "huge", strconv.Itoa(size)), map[string]string{helperEnvironment: "1"}, "", 5*time.Second, limit)

	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Success {
		t.Fatalf("unexpected result: %+v", result)
	}
	stdout := bytes.Repeat([]byte{'A'}, size)
	stderr := bytes.Repeat([]byte{'B'}, size+11)
	assertEvidence(t, result.Stdout, stdout, limit)
	assertEvidence(t, result.Stderr, stderr, limit)
}

func TestRunRejectsDirtyPreflight(t *testing.T) {
	fixture := newGitFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.repo, "dirty.txt"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.runner.Run(context.Background(), helperPolicy(t, "success", time.Second, 128), fixture.request)
	if !errors.Is(err, verification.ErrPreflight) {
		t.Fatalf("expected preflight error, got %v", err)
	}
	if result.Executed || result.Success || result.Failure != verification.FailurePreflight {
		t.Fatalf("dirty repository executed command: %+v", result)
	}
}

func TestRunDetectsRepositoryMutation(t *testing.T) {
	tests := []struct {
		name   string
		action string
		extra  func(*gitFixture) []string
		check  func(*testing.T, *gitFixture)
	}{
		{
			name:   "tracked",
			action: "mutate-tracked",
			check: func(t *testing.T, fixture *gitFixture) {
				contents, err := os.ReadFile(filepath.Join(fixture.repo, "tracked.txt"))
				if err != nil || string(contents) != "mutated\n" {
					t.Fatalf("mutation was reverted: %q, %v", contents, err)
				}
			},
		},
		{
			name:   "untracked",
			action: "mutate-untracked",
			check: func(t *testing.T, fixture *gitFixture) {
				if _, err := os.Stat(filepath.Join(fixture.repo, "created.txt")); err != nil {
					t.Fatalf("mutation was reverted: %v", err)
				}
			},
		},
		{
			name:   "head",
			action: "mutate-head",
			extra:  func(fixture *gitFixture) []string { return []string{fixture.git, string(fixture.base)} },
			check: func(t *testing.T, fixture *gitFixture) {
				if got := gitOutput(t, fixture.git, fixture.repo, "rev-parse", "HEAD"); got != string(fixture.base) {
					t.Fatalf("HEAD was reverted: %s", got)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			extra := []string(nil)
			if test.extra != nil {
				extra = test.extra(fixture)
			}
			argv := helperArgv(t, append([]string{test.action}, extra...)...)
			policy := newPolicy(t, argv, map[string]string{helperEnvironment: "1"}, "", 5*time.Second, 128)
			result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
			if !errors.Is(err, verification.ErrIntegrity) {
				t.Fatalf("expected integrity error, got %v", err)
			}
			if result.Success || !result.IntegrityError || result.Failure != verification.FailureIntegrity {
				t.Fatalf("unexpected integrity result: %+v", result)
			}
			test.check(t, fixture)
		})
	}
}

func TestRunKillsBackgroundProcessBeforePostflight(t *testing.T) {
	fixture := newGitFixture(t)
	mutation := filepath.Join(fixture.repo, "delayed.txt")
	policy := newPolicy(t, helperArgv(t, "background-mutation", mutation),
		map[string]string{helperEnvironment: "1"}, "", 5*time.Second, 128)
	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if err != nil || !result.Success {
		t.Fatalf("Run: result=%+v err=%v", result, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(mutation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("background process survived verification: %v", err)
	}
}

func TestRunBoundsOutputDrainFromEscapedProcess(t *testing.T) {
	fixture := newGitFixture(t)
	marker := filepath.Join(t.TempDir(), "escaped")
	policy := newPolicy(t, helperArgv(t, "background-escaped", immutableTestExecutable, marker),
		map[string]string{helperEnvironment: "1"}, "", 5*time.Second, 128)
	started := time.Now()
	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if !errors.Is(err, verification.ErrIntegrity) || !result.IntegrityError || result.Failure != verification.FailureIntegrity {
		t.Fatalf("Run: result=%+v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("escaped output descriptor blocked verification for %v", elapsed)
	}
}

func TestRunKillsBackgroundProcessFromSuccessfulGit(t *testing.T) {
	fixture := newGitFixture(t)
	mutation := filepath.Join(t.TempDir(), "delayed-git-mutation")
	runner, err := verification.NewRunner(verification.RunnerConfig{
		GitExecutable: immutableTestExecutable,
		GitTimeout:    5 * time.Second,
		Environment: withHelperCoverageEnvironment(map[string]string{
			fakeGitExecutableEnvironment: fixture.git,
			fakeGitMutationEnvironment:   mutation,
		}),
		Name: "fake-git", Version: "v1", ImageDigest: "sha256:fake-git",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), helperPolicy(t, "success", 5*time.Second, 128), fixture.request)
	if err != nil || !result.Success {
		t.Fatalf("Run: result=%+v err=%v", result, err)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(mutation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("background Git process survived invocation: %v", err)
	}
}

func TestRunPinsGitContent(t *testing.T) {
	fixture := newGitFixture(t)
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeGit := filepath.Join(directory, "git")
	if err := copyNativeExecutable(immutableTestExecutable, fakeGit); err != nil {
		t.Fatal(err)
	}
	runner, err := verification.NewRunner(verification.RunnerConfig{GitExecutable: fakeGit, GitTimeout: 5 * time.Second,
		Environment: withHelperCoverageEnvironment(map[string]string{fakeGitExecutableEnvironment: fixture.git}),
		Name:        "fake-git", Version: "v1", ImageDigest: "sha256:fake-git"})
	if err != nil {
		t.Fatal(err)
	}
	mutateExecutable(t, fakeGit)
	result, err := runner.Run(context.Background(), helperPolicy(t, "success", time.Second, 128), fixture.request)
	if !errors.Is(err, verification.ErrPreflight) || result.Executed || result.Failure != verification.FailurePreflight {
		t.Fatalf("mutated Git executable was accepted: result=%+v err=%v", result, err)
	}
}

func TestWorkingDirectoryAndRepositoryPathValidation(t *testing.T) {
	fixture := newGitFixture(t)
	for _, directory := range []string{"../escape", "sub/../escape", filepath.Join(fixture.repo, "sub")} {
		t.Run(strings.ReplaceAll(directory, string(filepath.Separator), "_"), func(t *testing.T) {
			_, err := verification.NewPolicy(verification.PolicyConfig{
				CheckName: "unit.check", Argv: helperArgv(t, "success"), WorkingDirectory: directory,
				Timeout: time.Second, Environment: map[string]string{helperEnvironment: "1"}, OutputBytes: 128,
			})
			if !errors.Is(err, verification.ErrInvalidPolicy) {
				t.Fatalf("expected invalid policy, got %v", err)
			}
		})
	}

	t.Run("valid subdirectory", func(t *testing.T) {
		policy := newPolicy(t, helperArgv(t, "print-cwd"), map[string]string{helperEnvironment: "1"}, "sub", 5*time.Second, 1024)
		result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
		if err != nil || !result.Success {
			t.Fatalf("Run: result=%+v err=%v", result, err)
		}
		expected, err := filepath.EvalSymlinks(filepath.Join(fixture.repo, "sub"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(result.Stdout.Prefix)) != expected {
			t.Fatalf("cwd = %q, want %q", result.Stdout.Prefix, expected)
		}
	})

	t.Run("symlink working directory", func(t *testing.T) {
		if err := os.Symlink("sub", filepath.Join(fixture.repo, "linked-sub")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		policy := newPolicy(t, helperArgv(t, "success"), map[string]string{helperEnvironment: "1"}, "linked-sub", time.Second, 128)
		result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
		if !errors.Is(err, verification.ErrInvalidRequest) || result.Executed {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("symlink repository", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "repository-link")
		if err := os.Symlink(fixture.repo, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		request := fixture.request
		request.RepositoryPath = link
		result, err := fixture.runner.Run(context.Background(), helperPolicy(t, "success", time.Second, 128), request)
		if !errors.Is(err, verification.ErrInvalidRequest) || result.Executed {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

func TestEnvironmentDoesNotInherit(t *testing.T) {
	t.Setenv("AMBIENT_SECRET", "must-not-leak")
	fixture := newGitFixtureWithEnvironment(t, map[string]string{"RUNNER_VALUE": "runner"})
	policy := newPolicy(t, helperArgv(t, "environment"), map[string]string{
		helperEnvironment: "1",
		"POLICY_VALUE":    "policy",
	}, "", 5*time.Second, 1024)

	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if err != nil || !result.Success {
		t.Fatalf("Run: result=%+v err=%v", result, err)
	}
	want := "ambient=\npolicy=policy\nrunner=runner\n"
	if string(result.Stdout.Prefix) != want {
		t.Fatalf("environment output = %q, want %q", result.Stdout.Prefix, want)
	}
}

func TestEmptyEnvironmentDoesNotInherit(t *testing.T) {
	t.Setenv("AMBIENT_SECRET", "must-not-leak")
	fixture := newGitFixture(t)
	policy := newPolicy(t, helperArgv(t, "environment"), nil, "", 5*time.Second, 1024)

	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if err != nil || !result.Success {
		t.Fatalf("Run: result=%+v err=%v", result, err)
	}
	want := "ambient=\npolicy=\nrunner=\n"
	if string(result.Stdout.Prefix) != want {
		t.Fatalf("environment output = %q, want %q", result.Stdout.Prefix, want)
	}
}

func TestErrorsNeverContainOutput(t *testing.T) {
	fixture := newGitFixture(t)
	const secret = "command-secret-that-must-not-enter-errors"
	policy := newPolicy(t, helperArgv(t, "secret-and-mutate", secret), map[string]string{helperEnvironment: "1"}, "", 5*time.Second, 8)

	result, err := fixture.runner.Run(context.Background(), policy, fixture.request)
	if !errors.Is(err, verification.ErrIntegrity) {
		t.Fatalf("expected integrity error, got %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), string(result.Stdout.Prefix)) {
		t.Fatalf("error contains command output: %q", err)
	}
	if len(result.Stdout.Prefix) != 8 || !result.Stdout.Truncated || result.Stdout.ByteCount != int64(len(secret)) {
		t.Fatalf("output was not bounded: %+v", result.Stdout)
	}
}

func TestRequestValidatesResultTuple(t *testing.T) {
	fixture := newGitFixture(t)
	result := task.ResultTuple{
		RepositoryTuple: task.RepositoryTuple{RepositoryID: fixture.request.RepositoryID, BaseSHA: fixture.base},
		ResultCommit:    fixture.result, Outcome: task.ResultChanged, ManifestEntries: 1, WorktreeClean: true,
	}
	if err := fixture.request.ValidateResult(result); err != nil {
		t.Fatalf("ValidateResult: %v", err)
	}
	result.ResultCommit = fixture.base
	if err := fixture.request.ValidateResult(result); !errors.Is(err, verification.ErrInvalidRequest) {
		t.Fatalf("expected mismatch rejection, got %v", err)
	}
}

func TestRequestRejectsRepositoryIdentityOutsideSQLiteRange(t *testing.T) {
	fixture := newGitFixture(t)
	request := fixture.request
	request.RepositoryID = task.RepositoryID(uint64(1 << 63))
	result, err := fixture.runner.Run(context.Background(), helperPolicy(t, "success", time.Second, 1024), request)
	if !errors.Is(err, verification.ErrInvalidRequest) || result.Executed {
		t.Fatalf("Run() result=%+v error=%v", result, err)
	}
}

func TestPolicyValidation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invalid := []verification.PolicyConfig{
		{CheckName: "Unstable Name", Argv: []string{executable}, Timeout: time.Second, OutputBytes: 1},
		{CheckName: "check", Argv: []string{"relative"}, Timeout: time.Second, OutputBytes: 1},
		{CheckName: "check", Argv: []string{executable}, Timeout: 0, OutputBytes: 1},
		{CheckName: "check", Argv: []string{executable}, Timeout: verification.MaxPolicyTimeout + 1, OutputBytes: 1},
		{CheckName: "check", Argv: []string{executable}, Timeout: time.Second, OutputBytes: verification.MaxOutputBytes + 1},
		{CheckName: "check", Argv: []string{executable}, Timeout: time.Second, OutputBytes: 1, Environment: map[string]string{"BAD-NAME": "x"}},
	}
	for i, config := range invalid {
		if _, err := verification.NewPolicy(config); !errors.Is(err, verification.ErrInvalidPolicy) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestPolicyAndRunnerSnapshotsAreCanonicalAndDetached(t *testing.T) {
	executable := immutableTestExecutable
	first, err := verification.NewPolicy(verification.PolicyConfig{CheckName: "snapshot", Argv: []string{executable, "arg"},
		Timeout: time.Second, OutputBytes: 64, Environment: map[string]string{"Z": "last", "A": "first"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := verification.NewPolicy(verification.PolicyConfig{CheckName: "snapshot", Argv: []string{executable, "arg"},
		Timeout: time.Second, OutputBytes: 64, Environment: map[string]string{"A": "first", "Z": "last"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256() != second.SHA256() || first.EnvironmentSHA256() != second.EnvironmentSHA256() {
		t.Fatal("canonical digests depend on map iteration order")
	}
	snapshot := first.Snapshot()
	snapshot.Argv[0] = "changed"
	if first.Argv()[0] != executable || snapshot.SHA256 != first.SHA256() {
		t.Fatal("policy snapshot was not detached")
	}
	if snapshot.ExecutableSHA256 == ([sha256.Size]byte{}) {
		t.Fatal("policy snapshot omitted executable digest")
	}

	git := findGit(t)
	runnerEnvironment := map[string]string{"RUNNER": "one", "SECOND": "two"}
	runner, err := verification.NewRunner(verification.RunnerConfig{GitExecutable: git, GitTimeout: time.Second,
		Environment: runnerEnvironment, Name: "fern", Version: "v1", ImageDigest: "sha256:image"})
	if err != nil {
		t.Fatal(err)
	}
	runnerEnvironment["RUNNER"] = "mutated"
	runnerSnapshot, err := runner.Snapshot(first)
	if err != nil {
		t.Fatal(err)
	}
	if runnerSnapshot.Name != "fern" || runnerSnapshot.Version != "v1" || runnerSnapshot.ImageDigest != "sha256:image" ||
		runnerSnapshot.EnvironmentSHA256 == first.EnvironmentSHA256() || runnerSnapshot.SHA256 == ([sha256.Size]byte{}) ||
		runnerSnapshot.GitExecutableSHA256 == ([sha256.Size]byte{}) {
		t.Fatalf("unexpected runner snapshot: %+v", runnerSnapshot)
	}
	secondRunner, err := verification.NewRunner(verification.RunnerConfig{GitExecutable: git, GitTimeout: time.Second,
		Environment: map[string]string{"SECOND": "two", "RUNNER": "one"}, Name: "fern", Version: "v1", ImageDigest: "sha256:image"})
	if err != nil {
		t.Fatal(err)
	}
	secondSnapshot, err := secondRunner.Snapshot(second)
	if err != nil || secondSnapshot != runnerSnapshot {
		t.Fatalf("merged environment digest is not canonical: %+v %+v %v", runnerSnapshot, secondSnapshot, err)
	}
}

func TestPolicyDigestPinsExecutableContent(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "check")
	if err := copyNativeExecutable(immutableTestExecutable, executable); err != nil {
		t.Fatal(err)
	}
	config := verification.PolicyConfig{CheckName: "check", Argv: []string{executable}, Timeout: time.Second, OutputBytes: 64}
	first, err := verification.NewPolicy(config)
	if err != nil {
		t.Fatal(err)
	}
	mutateExecutable(t, executable)
	second, err := verification.NewPolicy(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Snapshot().ExecutableSHA256 == second.Snapshot().ExecutableSHA256 || first.SHA256() == second.SHA256() {
		t.Fatal("policy digest did not bind executable content")
	}
}

func TestNewRunnerRejectsUnsafeGitExecutable(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "git")
	if err := copyNativeExecutable(immutableTestExecutable, executable); err != nil {
		t.Fatal(err)
	}
	valid := verification.RunnerConfig{GitExecutable: executable, GitTimeout: 5 * time.Second,
		Name: "fern", Version: "v1", ImageDigest: "sha256:image"}
	if _, err := verification.NewRunner(valid); err != nil {
		t.Fatalf("private executable rejected: %v", err)
	}
	if err := os.Chmod(executable, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := verification.NewRunner(valid); !errors.Is(err, verification.ErrInvalidRunner) {
		t.Fatalf("owner-writable executable error = %v", err)
	}
	if err := os.Chmod(executable, 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(executable, 0520); err != nil {
		t.Fatal(err)
	}
	if _, err := verification.NewRunner(valid); !errors.Is(err, verification.ErrInvalidRunner) {
		t.Fatalf("group-writable executable error = %v", err)
	}
	if err := os.Chmod(executable, 0502); err != nil {
		t.Fatal(err)
	}
	if _, err := verification.NewRunner(valid); !errors.Is(err, verification.ErrInvalidRunner) {
		t.Fatalf("world-writable executable error = %v", err)
	}
	if err := os.Chmod(executable, 0500); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "git-link")
	if err := os.Symlink(executable, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	valid.GitExecutable = link
	if _, err := verification.NewRunner(valid); !errors.Is(err, verification.ErrInvalidRunner) {
		t.Fatalf("symlinked executable error = %v", err)
	}
}

func TestNewPolicyRejectsUnsafeCheckExecutable(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "check")
	if err := copyNativeExecutable(immutableTestExecutable, executable); err != nil {
		t.Fatal(err)
	}
	config := verification.PolicyConfig{CheckName: "check", Argv: []string{executable}, Timeout: time.Second, OutputBytes: 64}
	if _, err := verification.NewPolicy(config); err != nil {
		t.Fatalf("private executable rejected: %v", err)
	}
	if err := os.Chmod(executable, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := verification.NewPolicy(config); !errors.Is(err, verification.ErrInvalidPolicy) {
		t.Fatalf("owner-writable executable error = %v", err)
	}
	if err := os.Chmod(executable, 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(executable, 0520); err != nil {
		t.Fatal(err)
	}
	if _, err := verification.NewPolicy(config); !errors.Is(err, verification.ErrInvalidPolicy) {
		t.Fatalf("writable executable error = %v", err)
	}
	if err := os.Chmod(executable, 0500); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "check-link")
	if err := os.Symlink(executable, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	config.Argv[0] = link
	if _, err := verification.NewPolicy(config); !errors.Is(err, verification.ErrInvalidPolicy) {
		t.Fatalf("symlinked executable error = %v", err)
	}
	parentLink := filepath.Join(directory, "parent-link")
	if err := os.Symlink(directory, parentLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	config.Argv[0] = filepath.Join(parentLink, "check")
	if _, err := verification.NewPolicy(config); !errors.Is(err, verification.ErrInvalidPolicy) {
		t.Fatalf("symlinked parent error = %v", err)
	}
	stickyParent := filepath.Join(directory, "sticky-parent")
	if err := os.Mkdir(stickyParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stickyParent, 01777); err != nil {
		t.Fatal(err)
	}
	stickyExecutable := filepath.Join(stickyParent, "check")
	if err := copyNativeExecutable(immutableTestExecutable, stickyExecutable); err != nil {
		t.Fatal(err)
	}
	config.Argv[0] = stickyExecutable
	if _, err := verification.NewPolicy(config); !errors.Is(err, verification.ErrInvalidPolicy) {
		t.Fatalf("user-owned sticky parent error = %v", err)
	}
}

func TestHelperProcess(t *testing.T) {
	separator := -1
	for i, argument := range os.Args {
		if argument == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+2 >= len(os.Args) || os.Args[separator+1] != "fern-verification-helper" {
		return
	}
	arguments := os.Args[separator+2:]
	switch arguments[0] {
	case "success":
		fmt.Fprint(os.Stdout, "stdout-success\n")
		fmt.Fprint(os.Stderr, "stderr-success\n")
	case "failure":
		fmt.Fprint(os.Stdout, "failure-output\n")
		fmt.Fprint(os.Stderr, "failure-error\n")
		os.Exit(7)
	case "sleep":
		fmt.Fprint(os.Stdout, "sleeping\n")
		time.Sleep(30 * time.Second)
	case "sleep-signal":
		if err := os.WriteFile(arguments[1], []byte("started"), 0600); err != nil {
			os.Exit(98)
		}
		time.Sleep(30 * time.Second)
	case "argv0":
		if os.Args[0] != arguments[1] {
			os.Exit(99)
		}
	case "huge":
		size, _ := strconv.Atoi(arguments[1])
		writeRepeated(os.Stdout, 'A', size)
		writeRepeated(os.Stderr, 'B', size+11)
	case "mutate-tracked":
		if err := os.WriteFile("tracked.txt", []byte("mutated\n"), 0600); err != nil {
			os.Exit(91)
		}
	case "mutate-untracked":
		if err := os.WriteFile("created.txt", []byte("created\n"), 0600); err != nil {
			os.Exit(92)
		}
	case "mutate-head":
		command := exec.Command(arguments[1], "reset", "--hard", arguments[2])
		command.Env = []string{}
		if err := command.Run(); err != nil {
			os.Exit(93)
		}
	case "print-cwd":
		directory, err := os.Getwd()
		if err != nil {
			os.Exit(94)
		}
		fmt.Fprintln(os.Stdout, directory)
	case "environment":
		fmt.Fprintf(os.Stdout, "ambient=%s\npolicy=%s\nrunner=%s\n", os.Getenv("AMBIENT_SECRET"), os.Getenv("POLICY_VALUE"), os.Getenv("RUNNER_VALUE"))
	case "secret-and-mutate":
		fmt.Fprint(os.Stdout, arguments[1])
		if err := os.WriteFile("secret-mutation.txt", []byte("x"), 0600); err != nil {
			os.Exit(95)
		}
	case "background-mutation":
		command := exec.Command("/bin/sh", "-c", "sleep 3; printf 'late\\n' > \"$1\"", "fern-background", arguments[1])
		command.Env = os.Environ()
		if err := command.Start(); err != nil {
			os.Exit(90)
		}
	case "background-escaped":
		command := exec.Command(arguments[1], "-test.run=^TestHelperProcess$", "--", "fern-verification-helper", "escaped-output", arguments[2])
		command.Env = helperProcessEnvironment()
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			os.Exit(88)
		}
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(arguments[2]); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := os.Stat(arguments[2]); err != nil {
			os.Exit(83)
		}
	case "escaped-output":
		if err := escapeProcessGroup(); err != nil {
			os.Exit(85)
		}
		if err := os.WriteFile(arguments[1], []byte("ready"), 0600); err != nil {
			os.Exit(84)
		}
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			if _, err := fmt.Fprint(os.Stdout, "."); err != nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
	default:
		os.Exit(96)
	}
	os.Exit(0)
}

func runFakeGit() int {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stdout, "git version verification-fake")
		return 0
	}
	realGit := os.Getenv(fakeGitExecutableEnvironment)
	command := exec.Command(realGit, os.Args[1:]...)
	command.Env = environmentWithout(os.Environ(), fakeGitExecutableEnvironment, fakeGitMutationEnvironment)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if command.ProcessState != nil {
			return command.ProcessState.ExitCode()
		}
		return 87
	}
	if mutation := os.Getenv(fakeGitMutationEnvironment); mutation != "" {
		background := exec.Command("/bin/sh", "-c", "sleep 3; printf 'late\\n' > \"$1\"", "fern-git-background", mutation)
		background.Env = environmentWithout(os.Environ(), fakeGitExecutableEnvironment, fakeGitMutationEnvironment)
		if err := background.Start(); err != nil {
			return 86
		}
	}
	return 0
}

func environmentWithout(environment []string, names ...string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		keep := true
		for _, name := range names {
			if strings.HasPrefix(entry, name+"=") {
				keep = false
				break
			}
		}
		if keep {
			result = append(result, entry)
		}
	}
	return result
}

func newGitFixture(t *testing.T) *gitFixture { return newGitFixtureWithEnvironment(t, nil) }

func newGitFixtureWithEnvironment(t *testing.T, environment map[string]string) *gitFixture {
	t.Helper()
	git := findGit(t)
	repo := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repo, "init", "--object-format=sha1", "--quiet")
	runGit(t, git, repo, "config", "user.name", "Verification Test")
	runGit(t, git, repo, "config", "user.email", "verification@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "kept.txt"), []byte("kept\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repo, "add", "tracked.txt", "sub/kept.txt")
	runGit(t, git, repo, "commit", "--quiet", "-m", "base")
	base := task.GitOID(gitOutput(t, git, repo, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("result\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repo, "add", "tracked.txt")
	runGit(t, git, repo, "commit", "--quiet", "-m", "result")
	result := task.GitOID(gitOutput(t, git, repo, "rev-parse", "HEAD"))
	runner, err := verification.NewRunner(verification.RunnerConfig{GitExecutable: git, GitTimeout: 5 * time.Second, Environment: withHelperCoverageEnvironment(environment),
		Name: "fern-verifier", Version: "v1", ImageDigest: "sha256:test-image"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return &gitFixture{
		git: git, repo: repo, base: base, result: result, runner: runner,
		request: verification.Request{RepositoryID: 42, BaseSHA: base, ResultCommit: result, RepositoryPath: repo},
	}
}

func findGit(t *testing.T) string {
	t.Helper()
	path := ""
	if runtime.GOOS == "darwin" {
		for _, candidate := range []string{"/Library/Developer/CommandLineTools/usr/bin/git", "/Applications/Xcode.app/Contents/Developer/usr/bin/git"} {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				path = candidate
				break
			}
		}
	}
	var err error
	if path == "" {
		path, err = exec.LookPath("git")
	}
	if err != nil {
		t.Skip("git is required")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(path)
}

func runGit(t *testing.T, git, repo string, arguments ...string) {
	t.Helper()
	args := append([]string{"-C", repo}, arguments...)
	command := exec.Command(git, args...)
	command.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2001-02-03T04:05:06Z", "GIT_COMMITTER_DATE=2001-02-03T04:05:06Z",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func gitOutput(t *testing.T, git, repo string, arguments ...string) string {
	t.Helper()
	args := append([]string{"-C", repo}, arguments...)
	output, err := exec.Command(git, args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", arguments, err)
	}
	return strings.TrimSpace(string(output))
}

func helperPolicy(t *testing.T, action string, timeout time.Duration, outputBytes int) verification.Policy {
	t.Helper()
	return newPolicy(t, helperArgv(t, action), map[string]string{helperEnvironment: "1"}, "", timeout, outputBytes)
}

func helperProcessEnvironment() []string {
	if coverageDirectory := os.Getenv("GOCOVERDIR"); coverageDirectory != "" {
		return []string{"GOCOVERDIR=" + coverageDirectory}
	}
	return nil
}

func withHelperCoverageEnvironment(environment map[string]string) map[string]string {
	coverageDirectory := os.Getenv("GOCOVERDIR")
	if coverageDirectory == "" {
		return environment
	}
	result := make(map[string]string, len(environment)+1)
	for name, value := range environment {
		result[name] = value
	}
	result["GOCOVERDIR"] = coverageDirectory
	return result
}

func helperArgv(t *testing.T, arguments ...string) []string {
	t.Helper()
	result := []string{immutableTestExecutable, "-test.run=^TestHelperProcess$", "--", "fern-verification-helper"}
	return append(result, arguments...)
}

func copyNativeExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func mutateExecutable(t *testing.T, path string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents[len(contents)-1] ^= 0xff
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0500); err != nil {
		t.Fatal(err)
	}
}

func newPolicy(t *testing.T, argv []string, environment map[string]string, directory string, timeout time.Duration, outputBytes int) verification.Policy {
	t.Helper()
	policy, err := verification.NewPolicy(verification.PolicyConfig{
		CheckName: "unit.check", Argv: argv, WorkingDirectory: directory,
		Timeout: timeout, Environment: environment, OutputBytes: outputBytes,
	})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return policy
}

func assertEvidence(t *testing.T, got verification.OutputEvidence, data []byte, limit int) {
	t.Helper()
	wantHash := sha256.Sum256(data)
	wantPrefix := data
	if len(wantPrefix) > limit {
		wantPrefix = wantPrefix[:limit]
	}
	if got.ByteCount != int64(len(data)) || got.SHA256 != wantHash || !bytes.Equal(got.Prefix, wantPrefix) || got.Truncated != (len(data) > limit) {
		t.Fatalf("evidence = %+v, want count=%d hash=%x prefix=%q truncated=%v", got, len(data), wantHash, wantPrefix, len(data) > limit)
	}
}

func writeRepeated(writer io.Writer, value byte, count int) {
	chunk := bytes.Repeat([]byte{value}, 4096)
	for count > 0 {
		n := count
		if n > len(chunk) {
			n = len(chunk)
		}
		if _, err := writer.Write(chunk[:n]); err != nil {
			os.Exit(97)
		}
		count -= n
	}
}
