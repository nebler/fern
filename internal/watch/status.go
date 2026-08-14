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

func AllSessionsIdle(ctx context.Context, ep runtime.Endpoint, auth runtime.ServerAuth) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ep.URL(), "/")+"/session/status", nil)
	if err != nil {
		return false, err
	}
	auth.Apply(req)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("query session status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("query session status: %s", resp.Status)
	}
	var statuses map[string]struct {
		Type string `json:"type"`
	}
	const maxStatusBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
	if err != nil {
		return false, fmt.Errorf("read session status: %w", err)
	}
	if len(body) > maxStatusBytes {
		return false, errors.New("decode session status: response exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&statuses); err != nil {
		return false, fmt.Errorf("decode session status: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("decode session status: multiple JSON values")
		}
		return false, fmt.Errorf("decode session status: trailing data: %w", err)
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
