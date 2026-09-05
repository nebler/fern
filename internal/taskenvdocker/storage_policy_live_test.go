package taskenvdocker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// This raw-Docker smoke checks the exact worker syscall/read-only/tmpfs policy,
// not project quotas. Its disposable managed volumes deliberately do not claim
// XFS coverage, and it works on Desktop without bypassing provider admission.
//
//	Run: FERN_STORAGE_POLICY_SMOKE_IMAGE=fern/opencode-background-source:signed \
//	     go test ./internal/taskenvdocker -run TestLiveWorkerStoragePolicy -v
func TestLiveWorkerStoragePolicy(t *testing.T) {
	image := os.Getenv("FERN_STORAGE_POLICY_SMOKE_IMAGE")
	if image == "" {
		t.Skip("set FERN_STORAGE_POLICY_SMOKE_IMAGE for explicit disposable Docker smoke")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}
	prefix := fmt.Sprintf("fern-storage-smoke-%d", time.Now().UnixNano())
	workspace, data := prefix+"-workspace", prefix+"-data"
	for _, name := range []string{workspace, data} {
		docker("volume", "create", name)
		t.Cleanup(func() {
			cleanupCtx, done := context.WithTimeout(context.Background(), 20*time.Second)
			defer done()
			out, err := exec.CommandContext(cleanupCtx, "docker", "volume", "rm", name).CombinedOutput()
			if err != nil {
				t.Errorf("remove own smoke volume %s: %v: %s", name, err, out)
			}
		})
	}
	profile := filepath.Join(t.TempDir(), "seccomp.json")
	security := workerSecurityOptions()
	if err := os.WriteFile(profile, []byte(strings.TrimPrefix(security[1], "seccomp=")), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--detach", "--name", prefix, "--read-only", "--user", containerUser,
		"--cap-drop", "ALL", "--security-opt", security[0], "--security-opt", "seccomp=" + profile,
		"--memory", "512m", "--memory-swap", "1g", "--pids-limit", "512", "--cpus", "2", "--init",
		"--ipc", "private", "--cgroupns", "private", "--shm-size", "64m", "--restart", "no",
		"--log-driver", "json-file", "--log-opt", "max-size=1m", "--log-opt", "max-file=3",
		"--publish", "127.0.0.1::4096", "--env", usernameEnv + "=smoke", "--env", passwordEnv + "=" + prefix,
		"--mount", "type=volume,source=" + workspace + ",target=" + workspaceTarget,
		"--mount", "type=volume,source=" + data + ",target=" + opencodeTarget,
	}
	tmpfs := workerTmpfs()
	paths := make([]string, 0, len(tmpfs))
	for path := range tmpfs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		args = append(args, "--tmpfs", path+":"+tmpfs[path])
	}
	args = append(args, image, "opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096")
	// Register removal before run: Docker may create a stopped container even
	// when the start syscall policy fails.
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		out, err := exec.CommandContext(cleanupCtx, "docker", "rm", "--force", prefix).CombinedOutput()
		if err != nil {
			t.Errorf("remove own smoke container %s: %v: %s", prefix, err, out)
		}
	})
	id := docker(args...)
	t.Logf("own disposable container: %s (%s)", id, prefix)
	port := docker("port", prefix, "4096/tcp")
	base := "http://" + port
	client := &http.Client{Timeout: 2 * time.Second}
	request := func(method, path string, auth bool, body string) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		if auth {
			req.SetBasicAuth("smoke", prefix)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return resp.StatusCode, string(data), err
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		status, body, err := request("GET", "/global/health", true, "")
		if err == nil && status == 200 && strings.Contains(body, `"healthy":true`) {
			t.Logf("authenticated health: %s", body)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health failed: status=%d body=%s err=%v; logs=%s", status, body, err, docker("logs", prefix))
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status, _, err := request("GET", "/global/health", false, ""); err != nil || status != 401 {
		t.Fatalf("unauthenticated health: status=%d err=%v", status, err)
	}
	output := docker("exec", prefix, "sh", "-ec", `git --version; gh --version; if gh api --help >/dev/null 2>&1; then echo 'gh unexpectedly accepted missing harness credentials'; exit 1; fi; mkdir /tmp/fern-smoke; cd /tmp/fern-smoke; git init -q; git config user.name smoke; git config user.email smoke@example.invalid; printf 'smoke\n' > README; git add README; git commit -qm smoke; git status --porcelain; git log -1 --format=%s; test ! -w /usr/local/bin`)
	t.Logf("git/gh and read-only-root smoke: %s", output)
	status, body, err := request("POST", "/session?directory=/tmp/fern-smoke", true, `{}`)
	if err != nil || status != 200 || !strings.Contains(body, `"id":"ses_`) {
		t.Fatalf("create session under worker policy: status=%d body=%s err=%v", status, body, err)
	}
	t.Log("session creation succeeded; no model/prompt or XFS qualification claimed")
}
