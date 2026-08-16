package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/runtime"
)

func TestCLIProcess(t *testing.T) {
	if os.Getenv("FERN_CLI_TEST_PROCESS") != "1" {
		return
	}
	separator := 0
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	os.Args = append([]string{"fern"}, os.Args[separator+1:]...)
	main()
	os.Exit(0)
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	commandArgs := append([]string{"-test.run=TestCLIProcess", "--"}, args...)
	command := exec.Command(os.Args[0], commandArgs...)
	command.Env = append(os.Environ(), "FERN_CLI_TEST_PROCESS=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run CLI: %v", err)
	}
	return stdout.String(), stderr.String(), exitError.ExitCode()
}

func TestHelpFormsSucceed(t *testing.T) {
	tests := [][]string{{"--help"}, {"-h"}, {"help"}, {"init", "--help"}, {"doctor", "--help"}, {"github", "--help"}, {"github", "publish", "--help"}, {"up", "--help"}, {"debug", "--help"}, {"help", "status"}, {"help", "debug", "events"}, {"help", "github", "publish"}}
	for _, args := range tests {
		stdout, stderr, code := runCLI(t, args...)
		if code != 0 || stdout == "" || stderr != "" {
			t.Fatalf("fern %v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		if !strings.Contains(stdout, "Usage:") {
			t.Fatalf("fern %v help missing usage: %q", args, stdout)
		}
	}
}

func TestVersionFlagSucceeds(t *testing.T) {
	stdout, stderr, code := runCLI(t, "--version")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "fern ") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestInvocationErrorsAreConcise(t *testing.T) {
	stdout, stderr, code := runCLI(t, "status", "--unknown")
	if code != 2 || stdout != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "fern: flag provided but not defined: -unknown") || strings.Contains(stderr, "level=ERROR") || strings.Contains(stderr, "time=") {
		t.Fatalf("unexpected diagnostic: %q", stderr)
	}
}

func TestUnknownCommandSuggestsCorrection(t *testing.T) {
	_, stderr, code := runCLI(t, "statsu")
	if code != 2 || !strings.Contains(stderr, `did you mean "status"?`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestStatusJSON(t *testing.T) {
	var output bytes.Buffer
	observation := runtime.Observation{State: runtime.StateFailed, DockerStatus: "exited", ExitCode: 137, OOMKilled: true}
	if err := writeStatusJSON(&output, "demo", observation); err != nil {
		t.Fatal(err)
	}
	var got statusJSON
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Workspace != "demo" || got.State != runtime.StateFailed || got.DockerStatus != "exited" || got.ExitCode != 137 || !got.OOMKilled {
		t.Fatalf("status JSON = %+v", got)
	}
}

func TestExitCodeClassification(t *testing.T) {
	if got := exitCode(invocationError{message: "bad flag"}); got != 2 {
		t.Fatalf("invocation exit code = %d, want 2", got)
	}
	if got := exitCode(commandExitError{err: errors.New("child failed"), code: 42}); got != 42 {
		t.Fatalf("child exit code = %d, want 42", got)
	}
	if got := exitCode(errors.New("runtime failure")); got != 1 {
		t.Fatalf("runtime exit code = %d, want 1", got)
	}
}
