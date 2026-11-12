// Package api defines the daemon's socket protocol and its client. The
// server is separate so clients do not load the daemon's dependencies.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/coder/websocket"

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

// Prebuild sends checkout, a tar stream of repository repo's files, to be
// built into its golden snapshot ahead of its first worktree. It returns
// before the build.
func (c *Client) Prebuild(ctx context.Context, repo string, checkout io.Reader) error {
	return c.do(ctx, http.MethodPut, "/goldens/"+url.PathEscape(repo), checkout, "application/x-tar", nil)
}

// GitHubToken returns a token for GitHub repository owner/repo from the
// daemon's GitHub App (internal/github). An *Error with status 404 means no
// App is set up, or it isn't installed on the repository.
func (c *Client) GitHubToken(ctx context.Context, repository string) (string, error) {
	body, err := json.Marshal(map[string]string{"repository": repository})
	if err != nil {
		return "", err
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/github/token", bytes.NewReader(body), "application/json", &t); err != nil {
		return "", err
	}
	return t.Token, nil
}

// Backup makes a backup of the node's threads now, as the hourly ones do,
// and returns once it is done. An *Error with status 404 means no backups
// are set up.
func (c *Client) Backup(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/backup", nil, "", nil)
}

// Disk returns what uses the node's disk. The daemon asks btrfs for every
// environment's size: seconds on a pool with large workspaces.
func (c *Client) Disk(ctx context.Context) (Disk, error) {
	var d Disk
	return d, c.do(ctx, http.MethodGet, "/disk", nil, "", &d)
}

// GC removes from the node's disk what nothing needs, and returns what it
// removed.
func (c *Client) GC(ctx context.Context) (Freed, error) {
	var f Freed
	return f, c.do(ctx, http.MethodPost, "/gc", nil, "", &f)
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

// Fork makes count new environments from environment id's snapshot snapID,
// or from its workspace as it is if snapID is empty, and returns their IDs.
func (c *Client) Fork(ctx context.Context, id, snapID string, count int) ([]string, error) {
	body, err := json.Marshal(ForkRequest{Snapshot: snapID, Count: count})
	if err != nil {
		return nil, err
	}
	var out ForkResponse
	err = c.do(ctx, http.MethodPost, envPath(id)+"/fork", bytes.NewReader(body), "application/json", &out)
	return out.Forks, err
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

// Holds returns what holds environment id's box awake, and its agent box if
// it has one: what runs in each, and when it sleeps and why. It wakes
// neither.
func (c *Client) Holds(ctx context.Context, id string) ([]Holds, error) {
	var out []Holds
	err := c.do(ctx, http.MethodGet, envPath(id)+"/holds", nil, "", &out)
	return out, err
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

// exec runs req as a session, so that it carries on through a restart of the
// daemon, which is how Fern upgrades.
func (c *Client) exec(ctx context.Context, path, id string, req boxexec.Request, streams boxexec.Streams) (int, error) {
	dial := func(ctx context.Context) (*websocket.Conn, error) {
		return boxexec.Dial(ctx, "ws://fern"+path, "", c.hc)
	}
	conn, err := dial(ctx)
	if err != nil {
		return 0, execError(id, err)
	}
	req.Session = rand.Text()
	code, err := boxexec.RunSession(ctx, conn, req, streams, boxexec.Resume{Dial: dial, For: boxexec.DefaultGrace})
	var dialErr *boxexec.DialError
	if errors.As(err, &dialErr) {
		return 0, execError(id, err)
	}
	return code, err
}

// execError is the error of a connection to a box that couldn't open.
func execError(id string, err error) error {
	var dialErr *boxexec.DialError
	if errors.As(err, &dialErr) {
		return readError(dialErr.StatusCode, bytes.NewReader(dialErr.Body))
	}
	return fmt.Errorf("exec in %s: %w", id, err)
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
