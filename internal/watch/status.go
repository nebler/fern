package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

var statusHTTPClient = &http.Client{Timeout: 2 * time.Second}

// AllSessionsIdle authoritatively reports whether the OpenCode server has no
// active sessions, shells, PTYs, or pending requests. Probes run sequentially
// against one endpoint: worst case is six probes at statusHTTPClient's 2s
// timeout each (~12s) plus body reads, so callers must budget accordingly.
// Any probe error fails closed (not idle).
func AllSessionsIdle(ctx context.Context, ep runtime.Endpoint, auth runtime.ServerAuth) (bool, error) {
	checks := []struct {
		path   string
		decode func([]byte) (bool, error)
	}{
		{path: "/api/session/active", decode: decodeV2Active},
		{path: "/api/shell", decode: decodeV2PendingList},
		{path: "/api/pty", decode: decodeV2PTYs},
		{path: "/api/permission/request", decode: decodeV2PendingList},
		{path: "/api/form/request", decode: decodeV2PendingList},
		{path: "/api/question/request", decode: decodeV2PendingList},
	}
	for _, check := range checks {
		body, err := getStatus(ctx, ep, auth, check.path)
		if err != nil {
			return false, err
		}
		idle, err := check.decode(body)
		if err != nil || !idle {
			return idle, err
		}
	}
	return true, nil
}

func getStatus(ctx context.Context, ep runtime.Endpoint, auth runtime.ServerAuth, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ep.URL(), "/")+path, nil)
	if err != nil {
		return nil, err
	}
	auth.Apply(req)
	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query OpenCode activity %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("query OpenCode activity %s: %s", path, resp.Status)
	}
	const maxStatusBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OpenCode activity %s: %w", path, err)
	}
	if len(body) > maxStatusBytes {
		return nil, fmt.Errorf("decode OpenCode activity %s: response exceeds 1 MiB", path)
	}
	return body, nil
}

// decodeV2Active reports whether no active OpenCode session exists. Every map
// entry is validated before the count is decided: an unknown type fails closed
// regardless of Go's randomized map iteration order, so an unknown entry can
// never be skipped by an earlier return.
func decodeV2Active(body []byte) (bool, error) {
	var response struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := decodeStatusJSON(body, &response); err != nil {
		return false, err
	}
	if response.Data == nil {
		return false, errors.New("decode session status: expected data object")
	}
	active := 0
	for _, entry := range response.Data {
		switch entry.Type {
		case "running", "busy", "retry":
			active++
		default:
			return false, fmt.Errorf("decode session status: unknown V2 active type %q", entry.Type)
		}
	}
	return active == 0, nil
}

func decodeV2PTYs(body []byte) (bool, error) {
	var response struct {
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := decodeStatusJSON(body, &response); err != nil {
		return false, err
	}
	if response.Data == nil {
		return false, errors.New("decode PTY activity: expected data array")
	}
	for _, pty := range response.Data {
		switch pty.Status {
		case "running":
			return false, nil
		case "exited":
		default:
			return false, fmt.Errorf("decode PTY activity: unknown status %q", pty.Status)
		}
	}
	return true, nil
}

func decodeV2PendingList(body []byte) (bool, error) {
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := decodeStatusJSON(body, &response); err != nil {
		return false, err
	}
	if response.Data == nil {
		return false, errors.New("decode pending activity: expected data array")
	}
	return len(response.Data) == 0, nil
}

func decodeStatusJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode session status: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode session status: multiple JSON values")
		}
		return fmt.Errorf("decode session status: trailing data: %w", err)
	}
	return nil
}
