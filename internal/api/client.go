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

// Error codes.
const (
	CodeNotFound = "not_found"
	CodeInvalid  = "invalid"
	CodeFailed   = "failed"
)

// Error is a typed API error.
type Error struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) status() int {
	switch e.Code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeInvalid:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

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

// CreateOptions are the optional parts of a new environment.
type CreateOptions struct {
	Repo string    // where the code came from, for listings
	Path string    // the workspace, <pool>/worktrees/<repo>/<name>; see store.CreateOptions
	Seed io.Reader // a tar stream to fill the workspace with
}

// Create makes environment id. Creating one that exists returns it unchanged.
func (c *Client) Create(ctx context.Context, id string, opts CreateOptions) (Env, error) {
	path := "/envs/" + url.PathEscape(id)
	q := url.Values{}
	if opts.Repo != "" {
		q.Set("repo", opts.Repo)
	}
	if opts.Path != "" {
		q.Set("path", opts.Path)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	contentType := ""
	if opts.Seed != nil {
		contentType = "application/x-tar"
	}
	var env Env
	err := c.do(ctx, http.MethodPut, path, opts.Seed, contentType, &env)
	return env, err
}

// FindByPath returns the environment whose workspace is path or holds it.
func (c *Client) FindByPath(ctx context.Context, path string) (Env, error) {
	var envs []Env
	if err := c.do(ctx, http.MethodGet, "/envs?"+url.Values{"path": {path}}.Encode(), nil, "", &envs); err != nil {
		return Env{}, err
	}
	if len(envs) == 0 {
		return Env{}, &Error{Code: CodeNotFound, Message: "no environment at " + path}
	}
	return envs[0], nil
}

// Get returns environment id.
func (c *Client) Get(ctx context.Context, id string) (Env, error) {
	var env Env
	err := c.do(ctx, http.MethodGet, "/envs/"+url.PathEscape(id), nil, "", &env)
	return env, err
}

// List returns all environments.
func (c *Client) List(ctx context.Context) ([]Env, error) {
	var envs []Env
	err := c.do(ctx, http.MethodGet, "/envs", nil, "", &envs)
	return envs, err
}

// Delete removes environment id with its box and snapshots.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/envs/"+url.PathEscape(id), nil, "", nil)
}

// Snapshot takes snapshot snap of environment env; an empty snap gets a
// generated ID.
func (c *Client) Snapshot(ctx context.Context, env, snap string) (Snapshot, error) {
	var out Snapshot
	err := c.doJSON(ctx, http.MethodPost, "/envs/"+url.PathEscape(env)+"/snapshots", SnapshotRequest{ID: snap}, &out)
	return out, err
}

// ReplaceSnapshot takes snapshot snap of environment env, replacing one with
// the same ID.
func (c *Client) ReplaceSnapshot(ctx context.Context, env, snap string) (Snapshot, error) {
	var out Snapshot
	err := c.doJSON(ctx, http.MethodPost, "/envs/"+url.PathEscape(env)+"/snapshots", SnapshotRequest{ID: snap, Replace: true}, &out)
	return out, err
}

// DeleteSnapshot removes snapshot snap of environment env; removing one that
// doesn't exist is not an error.
func (c *Client) DeleteSnapshot(ctx context.Context, env, snap string) error {
	return c.do(ctx, http.MethodDelete, "/envs/"+url.PathEscape(env)+"/snapshots/"+url.PathEscape(snap), nil, "", nil)
}

// Snapshots lists environment env's snapshots, oldest first.
func (c *Client) Snapshots(ctx context.Context, env string) ([]Snapshot, error) {
	var out []Snapshot
	err := c.do(ctx, http.MethodGet, "/envs/"+url.PathEscape(env)+"/snapshots", nil, "", &out)
	return out, err
}

// Restore puts environment env to sleep and restores snapshot snap.
func (c *Client) Restore(ctx context.Context, env, snap string) error {
	return c.doJSON(ctx, http.MethodPost, "/envs/"+url.PathEscape(env)+"/restore", RestoreRequest{Snapshot: snap}, nil)
}

// Sleep stops environment env's box.
func (c *Client) Sleep(ctx context.Context, env string) error {
	return c.do(ctx, http.MethodPost, "/envs/"+url.PathEscape(env)+"/sleep", nil, "", nil)
}

// Wake starts environment env's box and waits until it is ready.
func (c *Client) Wake(ctx context.Context, env string) error {
	return c.do(ctx, http.MethodPost, "/envs/"+url.PathEscape(env)+"/wake", nil, "", nil)
}

// Exec runs req in environment env's box, waking it if needed, and returns
// the command's exit code. See boxexec.Run for the streams.
func (c *Client) Exec(ctx context.Context, env string, req boxexec.Request, streams boxexec.Streams) (int, error) {
	u := "ws://fern/envs/" + url.PathEscape(env) + "/exec"
	conn, err := boxexec.Dial(ctx, u, "", c.hc)
	if err != nil {
		var dialErr *boxexec.DialError
		if errors.As(err, &dialErr) {
			if apiErr := decodeError(bytes.NewReader(dialErr.Body)); apiErr != nil {
				return 0, apiErr
			}
		}
		return 0, fmt.Errorf("exec in %s: %w", env, err)
	}
	defer func() { _ = conn.CloseNow() }()
	return boxexec.Run(ctx, conn, req, streams)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, bytes.NewReader(body), "application/json", out)
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
		if apiErr := decodeError(resp.Body); apiErr != nil {
			return apiErr
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode reply: %w", method, path, err)
	}
	return nil
}

// decodeError reads a typed error from an answer's body, or returns nil if it
// has none.
func decodeError(body io.Reader) *Error {
	var e Error
	if err := json.NewDecoder(io.LimitReader(body, 1<<16)).Decode(&e); err == nil && e.Code != "" {
		return &e
	}
	return nil
}
