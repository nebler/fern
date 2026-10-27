// Package api defines the daemon's socket protocol and its client. The
// server is separate so clients do not load the daemon's dependencies.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/nebler/fern/internal/boxexec"
)

// Client talks to the daemon over its unix socket.
type Client struct {
	hc *http.Client
}

// NewClient returns a client for the daemon listening on socket.
func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{hc: &http.Client{Transport: tr}}
}

// base is the URL prefix for requests; the host is ignored by the transport.
const base = "http://fern"

// Create makes environment id, its workspace at path if path isn't empty
// (<pool>/worktrees/<repo>/<name>; a directory there is adopted), filled from
// the tar stream seed if seed isn't nil. Creating one that exists changes
// nothing.
func (c *Client) Create(ctx context.Context, id, path string, seed io.Reader) error {
	contentType := ""
	if seed != nil {
		contentType = "application/x-tar"
	}
	return c.do(ctx, http.MethodPut, envPath(id)+"?"+url.Values{"path": {path}}.Encode(), seed, contentType, nil)
}

// FindByPath returns the environment whose workspace is path or holds it.
func (c *Client) FindByPath(ctx context.Context, path string) (Env, error) {
	var envs []Env
	if err := c.do(ctx, http.MethodGet, "/envs?"+url.Values{"path": {path}}.Encode(), nil, "", &envs); err != nil {
		return Env{}, err
	}
	if len(envs) == 0 {
		return Env{}, &Error{Status: http.StatusNotFound, Message: "no environment at " + path}
	}
	return envs[0], nil
}

// List returns all environments.
func (c *Client) List(ctx context.Context) ([]Env, error) {
	var envs []Env
	err := c.do(ctx, http.MethodGet, "/envs", nil, "", &envs)
	return envs, err
}

// Delete removes environment id with its box and snapshots.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, envPath(id), nil, "", nil)
}

// Snapshot takes snapshot snapID of environment id. With replace, it takes
// it again if it exists.
func (c *Client) Snapshot(ctx context.Context, id, snapID string, replace bool) error {
	return c.doJSON(ctx, http.MethodPost, envPath(id)+"/snapshots", SnapshotRequest{ID: snapID, Replace: replace})
}

// DeleteSnapshot removes snapshot snapID of environment id; removing one
// that doesn't exist is not an error.
func (c *Client) DeleteSnapshot(ctx context.Context, id, snapID string) error {
	return c.do(ctx, http.MethodDelete, envPath(id)+"/snapshots/"+url.PathEscape(snapID), nil, "", nil)
}

// Snapshots lists environment id's snapshots, oldest first.
func (c *Client) Snapshots(ctx context.Context, id string) ([]Snapshot, error) {
	var out []Snapshot
	err := c.do(ctx, http.MethodGet, envPath(id)+"/snapshots", nil, "", &out)
	return out, err
}

// Restore puts environment id to sleep and restores snapshot snapID.
func (c *Client) Restore(ctx context.Context, id, snapID string) error {
	return c.doJSON(ctx, http.MethodPost, envPath(id)+"/restore", RestoreRequest{Snapshot: snapID})
}

// Sleep stops environment id's box.
func (c *Client) Sleep(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, envPath(id)+"/sleep", nil, "", nil)
}

// Wake starts environment id's box and waits until it is ready.
func (c *Client) Wake(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, envPath(id)+"/wake", nil, "", nil)
}

// Prewake asks for environment id's box to be woken in the background, and
// returns without waiting for it.
func (c *Client) Prewake(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, envPath(id)+"/prewake", nil, "", nil)
}

// Exec runs req in environment id's box, waking it if needed, and returns
// the command's exit code. See boxexec.Run for the streams.
func (c *Client) Exec(ctx context.Context, id string, req boxexec.Request, streams boxexec.Streams) (int, error) {
	return c.exec(ctx, envPath(id)+"/exec", id, req, streams)
}

// ExecThenSleep runs a command like Exec; once it ends, the box sleeps at
// once unless another command runs, instead of after the idle times. An
// agent stopped because it had nothing to do (package hibernate) ends so.
func (c *Client) ExecThenSleep(ctx context.Context, id string, req boxexec.Request, streams boxexec.Streams) (int, error) {
	return c.exec(ctx, envPath(id)+"/exec?then=sleep", id, req, streams)
}

// ExecAgent runs a command like ExecThenSleep in environment id's agent box,
// Claude's own (with the daemon's --agent-boxes; otherwise in its box). There
// Claude's Bash runs in the environment's box, with the services.
func (c *Client) ExecAgent(ctx context.Context, id string, req boxexec.Request, streams boxexec.Streams) (int, error) {
	return c.exec(ctx, envPath(id)+"/exec?box=agent&then=sleep", id, req, streams)
}

func (c *Client) exec(ctx context.Context, path, id string, req boxexec.Request, streams boxexec.Streams) (int, error) {
	conn, err := boxexec.Dial(ctx, "ws://fern"+path, "", c.hc)
	if err != nil {
		var dialErr *boxexec.DialError
		if errors.As(err, &dialErr) {
			return 0, readError(dialErr.StatusCode, bytes.NewReader(dialErr.Body))
		}
		return 0, fmt.Errorf("exec in %s: %w", id, err)
	}
	defer func() { _ = conn.CloseNow() }()
	return boxexec.Run(ctx, conn, req, streams)
}

func envPath(id string) string { return "/envs/" + url.PathEscape(id) }

func (c *Client) doJSON(ctx context.Context, method, path string, in any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, bytes.NewReader(body), "application/json", nil)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return readError(resp.StatusCode, resp.Body)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode reply: %w", method, path, err)
	}
	return nil
}
