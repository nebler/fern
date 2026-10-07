package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// printSerialDiagnostics writes best-effort evidence about a serial run to
// stderr after a failure: the durable run row, the fake provider's stats, the
// worker container's log tail, and the OpenCode session's view of the run.
func printSerialDiagnostics(cli *client.Client, databasePath, runID, providerEndpoint string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := os.Stderr
	fmt.Fprintln(out, "=== serial diagnostics ===")
	var state, phase, lastError, evidence, containerID, sessionID string
	var hostPort sql.NullInt64
	database, err := sql.Open("sqlite", databasePath)
	if err == nil {
		err = database.QueryRowContext(ctx, `SELECT state,effect_phase,COALESCE(last_error,''),COALESCE(last_evidence,''),COALESCE(observed_container_id,''),host_port,opencode_session_id FROM runs WHERE id=?`, runID).
			Scan(&state, &phase, &lastError, &evidence, &containerID, &hostPort, &sessionID)
		_ = database.Close()
	}
	fmt.Fprintf(out, "run row: state=%s phase=%s last_error=%q container=%s host_port=%v session=%s error=%v\nlast_evidence=%s\n",
		state, phase, lastError, containerID, hostPort.Int64, sessionID, err, evidence)
	if stats, err := readStats(providerEndpoint); err != nil {
		fmt.Fprintf(out, "provider stats error: %v\n", err)
	} else {
		fmt.Fprintf(out, "provider stats: calls=%d disconnects=%d requests=%d\n", stats.Calls, stats.Disconnects, len(stats.Requests))
	}
	if containerID == "" {
		return
	}
	if info, err := cli.ContainerInspect(ctx, containerID); err != nil {
		fmt.Fprintf(out, "container inspect error: %v\n", err)
	} else if info.State != nil {
		fmt.Fprintf(out, "container state: status=%s running=%t exit=%d oom=%t error=%q network=%s\n",
			info.State.Status, info.State.Running, info.State.ExitCode, info.State.OOMKilled, info.State.Error, info.HostConfig.NetworkMode)
	}
	if logs, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "80"}); err != nil {
		fmt.Fprintf(out, "container logs error: %v\n", err)
	} else {
		var buffer bytes.Buffer
		_, copyErr := stdcopy.StdCopy(&buffer, &buffer, io.LimitReader(logs, 1<<20))
		_ = logs.Close()
		fmt.Fprintf(out, "container logs (tail, error=%v):\n%s\n", copyErr, buffer.String())
	}
	printContainerExec(ctx, cli, containerID, `cd /home/user/workspace && ls -la && git rev-parse --git-dir --show-toplevel HEAD; `+
		`ls -la /home/user/.local/share/opencode /home/user/.local/share/opencode/log /home/user/.cache /home/user/.cache/opencode 2>&1 | head -60; `+
		`for f in $(ls -t /home/user/.local/share/opencode/log/*.log 2>/dev/null | head -2); do echo "--- $f"; tail -n 150 "$f"; done`)
	if !hostPort.Valid || sessionID == "" {
		return
	}
	username, password, err := runtimeCredentials(ctx, cli, containerID)
	if err != nil {
		fmt.Fprintf(out, "runtime credentials error: %v\n", err)
		return
	}
	endpoint := "http://127.0.0.1:" + strconv.FormatInt(hostPort.Int64, 10)
	for _, path := range []string{
		"/api/session/active",
		"/api/model",
		"/api/agent",
		"/api/session/" + sessionID,
		"/api/session/" + sessionID + "/history?after=0&limit=50",
		"/api/session/" + sessionID + "/message",
		"/session/" + sessionID + "/message",
	} {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
		request.SetBasicAuth(username, password)
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			fmt.Fprintf(out, "GET %s error: %v\n", path, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16<<10))
		_ = response.Body.Close()
		fmt.Fprintf(out, "GET %s status=%d body=%s\n", path, response.StatusCode, body)
	}
}

func printContainerExec(ctx context.Context, cli *client.Client, containerID, script string) {
	out := os.Stderr
	created, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User: "1001:1001", Cmd: []string{"/bin/sh", "-c", script}, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		fmt.Fprintf(out, "container exec create error: %v\n", err)
		return
	}
	attached, err := cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		fmt.Fprintf(out, "container exec attach error: %v\n", err)
		return
	}
	defer attached.Close()
	var buffer bytes.Buffer
	_, copyErr := stdcopy.StdCopy(&buffer, &buffer, io.LimitReader(attached.Reader, 256<<10))
	fmt.Fprintf(out, "container exec (error=%v):\n%s\n", copyErr, buffer.String())
}
