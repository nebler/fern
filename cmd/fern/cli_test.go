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
	tests := [][]string{{"--help"}, {"-h"}, {"help"}, {"init", "--help"}, {"doctor", "--help"}, {"github", "--help"}, {"github", "publish", "--help"}, {"up", "--help"}, {"backup", "--help"}, {"backup", "create", "--help"}, {"backup", "restore", "--help"}, {"debug", "--help"}, {"debug", "wake", "--help"}, {"debug", "quarantine-publications", "--help"}, {"help", "status"}, {"help", "backup", "create"}, {"help", "backup", "restore"}, {"help", "debug", "events"}, {"help", "debug", "wake"}, {"help", "debug", "quarantine-publications"}, {"help", "github", "publish"}}
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

func TestUsageTextIsDerivedUnchanged(t *testing.T) {
	t.Parallel()
	const want = `Fern supervises one durable OpenCode workspace in Docker.

Usage:
  fern <command> [flags]
  fern help [command]

Commands:
  init                           Create a secure phone-demo configuration
  doctor                         Verify host and private phone-demo readiness
  github                         Validate the retired standalone GitHub preflight
  up                             Run the workspace supervisor and authenticated proxy
  attach                         Open the official client through the Fern proxy
  status                         Show the workspace runtime state
  logs                           Stream workspace container logs
  down                           Remove workspace compute while retaining session data
  backup create                  Quiesce the workspace and create a verified backup
  backup restore                 Stage, verify, and activate a backup
  debug events                   Stream the backend activity events used by Fern
  debug wake                     Print the phase waterfall for one workspace wake
  debug quarantine-publications  Quarantine unresolved retired publication records
  version                        Print Fern version information

Examples:
  fern init --repo /path/to/repository
  fern up --config fern.yaml
  fern status --json
  fern attach

Run 'fern help <command>' for command flags.
Documentation: https://github.com/nebler/fern
`
	if usageText != want {
		t.Fatalf("derived usage text drifted:\n%s", usageText)
	}
}

func TestGroupedHelpIsDerivedUnchanged(t *testing.T) {
	t.Parallel()
	tests := map[string]struct{ overview, usage string }{
		"github": {
			overview: "Run the non-mutating legacy GitHub publication preflight.",
			usage:    "Usage:\n  fern github publish [flags]",
		},
		"debug": {
			overview: "Inspect Fern internals and run explicit offline repairs.",
			usage:    "Usage:\n  fern debug events [flags]\n  fern debug wake [flags]\n  fern debug quarantine-publications [flags]",
		},
		"backup": {
			overview: "Create and restore verified offline host backups.",
			usage:    "Usage:\n  fern backup create [flags]\n  fern backup restore [flags]",
		},
	}
	for name, want := range tests {
		entry := lookupCommand(name)
		if entry == nil {
			t.Fatalf("registry lost command %q", name)
		}
		if got := groupedHelp(entry); got != want.overview+"\n\n"+want.usage {
			t.Fatalf("groupedHelp(%q) =\n%s", name, got)
		}
		if got := subcommandUsage(entry); got != want.usage {
			t.Fatalf("subcommandUsage(%q) =\n%s\nwant\n%s", name, got, want.usage)
		}
	}
	version := lookupCommand("version")
	if got := groupedHelp(version); got != "Print Fern version information.\n\nUsage:\n  fern version" {
		t.Fatalf("version grouped help = %s", got)
	}
}
