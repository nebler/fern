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

func AllSessionsIdle(ctx context.Context, ep runtime.Endpoint, auth runtime.ServerAuth) (bool, error) {
	protocol := ep.Protocol.Normalize()
	if protocol == runtime.ProtocolV2 {
		return v2AllActivityIdle(ctx, ep, auth)
	}
	path := "/session/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ep.URL(), "/")+path, nil)
	if err != nil {
		return false, err
	}
	auth.ApplyFor(req, protocol)
	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("query session status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return false, fmt.Errorf("query session status: %s", resp.Status)
	}
	const maxStatusBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
	if err != nil {
		return false, fmt.Errorf("read session status: %w", err)
	}
	if len(body) > maxStatusBytes {
		return false, errors.New("decode session status: response exceeds 1 MiB")
	}
	return decodeV1Statuses(body)
}

func v2AllActivityIdle(ctx context.Context, ep runtime.Endpoint, auth runtime.ServerAuth) (bool, error) {
	checks := []struct {
		path   string
		decode func([]byte) (bool, error)
	}{
		{path: "/api/session/active", decode: decodeV2Active},
		{path: "/api/shell", decode: decodeV2Shells},
		{path: "/api/pty", decode: decodeV2PTYs},
		{path: "/api/permission/request", decode: decodeV2PendingList},
		{path: "/api/form/request", decode: decodeV2PendingList},
	}
	for _, check := range checks {
		body, err := getStatus(ctx, ep, auth, runtime.ProtocolV2, check.path)
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

func getStatus(ctx context.Context, ep runtime.Endpoint, auth runtime.ServerAuth, protocol runtime.Protocol, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ep.URL(), "/")+path, nil)
	if err != nil {
		return nil, err
	}
	auth.ApplyFor(req, protocol)
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

func decodeV1Statuses(body []byte) (bool, error) {
	var statuses map[string]struct {
		Type string `json:"type"`
	}
	if err := decodeStatusJSON(body, &statuses); err != nil {
		return false, err
	}
	if statuses == nil {
		return false, fmt.Errorf("decode session status: expected object, got null")
	}
	for _, status := range statuses {
		if status.Type != "idle" {
			return false, nil
		}
	}
	return true, nil
}

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
	for _, active := range response.Data {
		if active.Type != "running" {
			return false, fmt.Errorf("decode session status: unknown V2 active type %q", active.Type)
		}
		return false, nil
	}
	return true, nil
}

func decodeV2Shells(body []byte) (bool, error) {
	var response struct {
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := decodeStatusJSON(body, &response); err != nil {
		return false, err
	}
	if response.Data == nil {
		return false, errors.New("decode shell activity: expected data array")
	}
	for _, shell := range response.Data {
		if shell.Status != "running" {
			return false, fmt.Errorf("decode shell activity: unknown status %q", shell.Status)
		}
		return false, nil
	}
	return true, nil
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
