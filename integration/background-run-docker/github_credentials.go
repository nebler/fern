package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/nebler/fern/internal/githubapp"
)

// All credentials are dummy values. Never include captured credential output in
// errors, command arguments, container environment, or harness stdout.
var dummyGitHubTokens = []string{"ghs_fern_integration_dummy_first_token", "ghs_fern_integration_dummy_second_token"}

type fakeAppTokens struct{}

func (fakeAppTokens) AppToken(time.Time) (string, error) { return "fake.app.signature", nil }

type localGitHubTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t localGitHubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" {
		return nil, errors.New("unexpected fake GitHub destination")
	}
	copy := request.Clone(request.Context())
	copy.URL.Scheme, copy.URL.Host = t.target.Scheme, t.target.Host
	copy.Host = t.target.Host
	return t.base.RoundTrip(copy)
}

type gitHubFixture struct {
	server *httptest.Server
	source githubapp.InstallationTokenSource
	calls  atomic.Int32
}

func newGitHubFixture() (*gitHubFixture, error) {
	f := &gitHubFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/7/access_tokens" || r.Header.Get("Authorization") != "Bearer fake.app.signature" ||
			json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 42 ||
			len(body.Permissions) != 2 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" {
			http.Error(w, "invalid scoped token request", http.StatusBadRequest)
			return
		}
		n := int(f.calls.Add(1))
		if n > len(dummyGitHubTokens) {
			http.Error(w, "unexpected extra mint", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": dummyGitHubTokens[n-1], "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions":          map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"},
			"repositories":         []map[string]any{{"id": 42, "full_name": "fern-integration/background-run"}},
			"repository_selection": "selected",
		})
	}))
	target, err := url.Parse(f.server.URL)
	if err == nil {
		// No request can reach GitHub: the only transport destination is httptest.
		f.source, err = githubapp.NewClient(&http.Client{Transport: localGitHubTransport{target, f.server.Client().Transport}}, fakeAppTokens{})
	}
	if err != nil {
		f.server.Close()
		return nil, err
	}
	return f, nil
}

func privateExec(ctx context.Context, cli *client.Client, id string, args ...string) ([]byte, int, error) {
	execution, err := cli.ContainerExecCreate(ctx, id, container.ExecOptions{User: "1001:1001", WorkingDir: "/home/user/workspace", AttachStdout: true, AttachStderr: true, Cmd: args})
	if err != nil {
		return nil, 0, errors.New("create private credential assertion failed")
	}
	attached, err := cli.ContainerExecAttach(ctx, execution.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, 0, errors.New("attach private credential assertion failed")
	}
	defer attached.Close()
	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, attached.Reader); err != nil {
		return nil, 0, errors.New("read private credential assertion failed")
	}
	for {
		status, err := cli.ContainerExecInspect(ctx, execution.ID)
		if err != nil {
			return nil, 0, errors.New("inspect private credential assertion failed")
		}
		if !status.Running {
			return stdout.Bytes(), status.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func assertGitHubCredentials(ctx context.Context, cli *client.Client, id, hostRoot string, fixture *gitHubFixture, mint int) error {
	if int(fixture.calls.Load()) != mint {
		return errors.New("GitHub credential mint count differs")
	}
	if _, code, err := privateExec(ctx, cli, id, "gh", "--version"); err != nil || code != 0 {
		return errors.New("provisioned gh wrapper failed")
	}
	if output, code, err := privateExec(ctx, cli, id, "gh", "auth", "token"); err != nil || code == 0 || len(output) != 0 {
		return errors.New("gh auth guard failed")
	}
	checkFiles := `set -eu
test "$(id -u)" = 1001
for file in /home/user/.local/share/opencode/fern-github-token /home/user/.local/share/opencode/fern-github-repository; do
  test -f "$file" && test ! -L "$file" && test -r "$file"
  test "$(stat -c '%u:%g:%a' "$file")" = 1001:1001:600
done
test "$(cat /home/user/.local/share/opencode/fern-github-repository)" = fern-integration/background-run`
	if _, code, err := privateExec(ctx, cli, id, "/bin/sh", "-c", checkFiles); err != nil || code != 0 {
		return errors.New("private GitHub files ownership, permissions, or repository differ")
	}
	fill := `printf 'protocol=https\nhost=github.com\npath=fern-integration/background-run.git\n\n' | GIT_TERMINAL_PROMPT=0 git credential fill`
	output, code, err := privateExec(ctx, cli, id, "/bin/sh", "-c", fill)
	if err != nil || code != 0 || !bytes.Contains(output, []byte("password="+dummyGitHubTokens[mint-1]+"\n")) || !bytes.Contains(output, []byte("username=x-access-token\n")) {
		return errors.New("Git credential helper did not return current scoped token")
	}
	denied := strings.ReplaceAll(fill, "background-run.git", "other-repository.git")
	output, code, err = privateExec(ctx, cli, id, "/bin/sh", "-c", denied)
	if err != nil || code == 0 || bytes.Contains(output, []byte("password=")) {
		return errors.New("Git credential helper did not deny another repository")
	}
	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	for _, env := range info.Config.Env {
		name, _, _ := strings.Cut(env, "=")
		if name == "GH_TOKEN" || name == "GITHUB_TOKEN" || name == "GH_ENTERPRISE_TOKEN" || name == "GITHUB_ENTERPRISE_TOKEN" || containsDummySecret([]byte(env)) {
			return errors.New("credential present in container environment")
		}
	}
	logs, err := cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err
	}
	defer logs.Close()
	data, err := io.ReadAll(logs)
	if err != nil || containsDummySecret(data) {
		return errors.New("container logs unreadable or contain a credential")
	}
	return filepath.WalkDir(hostRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(entry.Name(), "fern-github-token") || strings.Contains(entry.Name(), "fern-github-repository") {
			return errors.New("runtime GitHub credential file escaped private volume")
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read host credential-leak assertion file: %w", err)
		}
		if containsDummySecret(data) {
			return errors.New("credential leaked into host state, logs, or clone")
		}
		return nil
	})
}

func containsDummySecret(data []byte) bool {
	for _, secret := range append([]string{"fake.app.signature"}, dummyGitHubTokens...) {
		if bytes.Contains(data, []byte(secret)) {
			return true
		}
	}
	return false
}
