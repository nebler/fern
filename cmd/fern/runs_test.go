package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

func TestSelectAttachRunUsesBoundedNumberedSelection(t *testing.T) {
	branch := "main"
	runs := []runSummary{
		{ID: task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000201"), State: "working", Repository: "owner/first"},
		{ID: task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000202"), State: "needs_you", Repository: "owner/second", Branch: &branch},
	}
	var output bytes.Buffer
	selected, err := selectAttachRun(strings.NewReader("2\n"), &output, runs)
	if err != nil || selected != runs[1].ID {
		t.Fatalf("selected=%q error=%v", selected, err)
	}
	if !strings.Contains(output.String(), string(runs[0].ID)) || !strings.Contains(output.String(), "owner/second") {
		t.Fatalf("picker output=%q", output.String())
	}
	if _, err := selectAttachRun(strings.NewReader("3\n"), &bytes.Buffer{}, runs); err == nil {
		t.Fatal("picker accepted an out-of-range selection")
	}
}

func TestActiveAndAttachableRunFiltering(t *testing.T) {
	states := []string{"queued", "setting_up", "working", "needs_you", "canceling", "uncertain", "result_ready", "failed", "cleanup_required"}
	runs := make([]runSummary, 0, len(states))
	for index, state := range states {
		attachable := state == "setting_up" || state == "working" || state == "needs_you" || state == "uncertain"
		runs = append(runs, runSummary{ID: task.TaskID("run-" + string(rune('a'+index))), State: state, Attachable: attachable})
	}
	active := activeRuns(runs)
	if got := statesOf(active); strings.Join(got, ",") != "queued,setting_up,working,needs_you,canceling,uncertain" {
		t.Fatalf("active states=%v", got)
	}
	attachable := attachableRuns(runs)
	if got := statesOf(attachable); strings.Join(got, ",") != "setting_up,working,needs_you,uncertain" {
		t.Fatalf("attachable states=%v", got)
	}
}

func TestLaunchOpenCodeAttachPassesCapabilityOnlyThroughEnvironment(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "opencode-test")
	arguments := filepath.Join(directory, "arguments")
	environment := filepath.Join(directory, "environment")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf '1.18.16\n'
  exit 0
fi
printf '%s\n' "$@" > "$FERN_TEST_ARGUMENTS"
printf '%s\n%s\n%s\n' "$OPENCODE_SERVER_USERNAME" "$OPENCODE_SERVER_PASSWORD" "${FERN_TOKEN-unset}" > "$FERN_TEST_ENVIRONMENT"
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FERN_TEST_ARGUMENTS", arguments)
	t.Setenv("FERN_TEST_ENVIRONMENT", environment)
	t.Setenv("FERN_TOKEN", "must-not-be-inherited")
	attachment := runAttachResponse{SessionID: task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef"), Username: "opencode"}
	attachment.Password = "short-lived-secret"
	if err := launchOpenCodeAttach(context.Background(), executable, "https://fern.example:8443", attachment); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(argv); got != "attach\nhttps://fern.example:8443\n--session\nses_0123456789abcdef0123456789abcdef\n--pure\n" || strings.Contains(got, "short-lived-secret") {
		t.Fatalf("arguments=%q", got)
	}
	env, err := os.ReadFile(environment)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(env); got != "opencode\nshort-lived-secret\nunset\n" {
		t.Fatalf("environment=%q", got)
	}
}

func TestLaunchOpenCodeAttachChecksVersionAndPropagatesExitCode(t *testing.T) {
	attachment := runAttachResponse{SessionID: task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef"),
		Username: "opencode", Password: "short-lived-secret"}
	for _, test := range []struct {
		name, script string
		code         int
	}{
		{"wrong-version", "if [ \"$1\" = --version ]; then echo 1.18.17; exit 0; fi\n", 1},
		{"attach-exit", "if [ \"$1\" = --version ]; then echo 1.18.16; exit 0; fi\nexit 23\n", 23},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "opencode")
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+test.script), 0o700); err != nil {
				t.Fatal(err)
			}
			err := launchOpenCodeAttach(context.Background(), executable, "https://fern.example:8443", attachment)
			if err == nil || exitCode(err) != test.code {
				t.Fatalf("error=%v exit=%d, want %d", err, exitCode(err), test.code)
			}
		})
	}
}

func TestRunCommandFlagParsing(t *testing.T) {
	options, positional, err := parseAttachFlags([]string{"--endpoint", "https://fern.example", "tsk_0198d34d-6a50-75fb-b1f2-000000000201"})
	if err != nil || options.endpoint != "https://fern.example" || len(positional) != 1 {
		t.Fatalf("options=%+v positional=%v error=%v", options, positional, err)
	}
	if _, err := parseRunsFlags([]string{"--json", "unexpected"}); err == nil {
		t.Fatal("runs accepted a positional argument")
	}
}

func TestRemoteConnectionUsesSeparateClientAPIAndValidatesAttachment(t *testing.T) {
	token := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	runID := task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000201")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "missing bearer", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/fern/api/v1/runs":
			_ = json.NewEncoder(writer).Encode(runListResponse{Runs: []runSummary{{ID: runID, State: "working", Repository: "owner/repository",
				Head: strings.Repeat("a", 40), Attachable: true}}})
		case "/fern/api/v1/runs/" + string(runID) + "/attach":
			_ = json.NewEncoder(writer).Encode(runAttachResponse{RunID: runID, URL: "https://127.0.0.1:8443",
				SessionID: task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef"), Username: "opencode",
				Password: token, ExpiresAt: time.Now().Add(time.Hour)})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	t.Setenv("FERN_TOKEN", token)
	connection, err := resolveRunConnection(context.Background(), runCLIOptions{endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if runs, err := connection.list(context.Background()); err != nil || len(runs) != 1 || runs[0].ID != runID {
		t.Fatalf("runs=%+v error=%v", runs, err)
	}
	if attachment, err := connection.attach(context.Background(), runID); err != nil || attachment.RunID != runID || attachment.Password != token {
		t.Fatalf("attachment=%+v error=%v", attachment, err)
	}
	if _, err := parseFernClientOrigin("https://fern.example/path"); err == nil {
		t.Fatal("remote endpoint accepted a non-root path")
	}
	if parsed, err := parseFernClientOrigin("http://127.0.0.1:8080"); err != nil || parsed.String() != "http://127.0.0.1:8080" {
		t.Fatalf("loopback endpoint=%v error=%v", parsed, err)
	}
}

func TestRunConnectionDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirected" {
			redirected = true
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection := &runConnection{apiOrigin: origin, client: &http.Client{Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := connection.get(context.Background(), "/start", &struct{}{}); err == nil || redirected {
		t.Fatalf("redirect error=%v followed=%t", err, redirected)
	}
}

func statesOf(runs []runSummary) []string {
	states := make([]string, 0, len(runs))
	for _, run := range runs {
		states = append(states, run.State)
	}
	return states
}
