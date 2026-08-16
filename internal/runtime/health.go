package runtime

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
)

var healthHTTPClient = &http.Client{Timeout: 2 * time.Second}

func WaitHealthy(ctx context.Context, ep Endpoint, auth ServerAuth, timeout time.Duration) error {
	_, err := WaitHealthyProtocol(ctx, ep, auth, auth.Protocol.Normalize(), timeout)
	return err
}

func WaitHealthyProtocol(ctx context.Context, ep Endpoint, auth ServerAuth, requested Protocol, timeout time.Duration) (Protocol, error) {
	return WaitHealthyURL(ctx, ep.URL(), auth, requested, timeout)
}

func WaitHealthyURL(ctx context.Context, baseURL string, auth ServerAuth, requested Protocol, timeout time.Duration) (Protocol, error) {
	if err := requested.Validate(); err != nil {
		return "", err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	protocols := []Protocol{requested.Normalize()}
	if requested.Normalize() == ProtocolAuto {
		protocols = []Protocol{ProtocolV2, ProtocolV1}
	}
	var lastErr error
	for {
		var healthy []Protocol
		var attemptErrors []error
		for _, protocol := range protocols {
			if err := checkHealth(ctx, baseURL, auth, protocol); err == nil {
				healthy = append(healthy, protocol)
			} else {
				attemptErrors = append(attemptErrors, err)
			}
		}
		if len(healthy) == 1 {
			return healthy[0], nil
		}
		if len(healthy) > 1 {
			return "", errors.New("OpenCode protocol detection is ambiguous; set workspace.opencode to v1 or v2")
		}
		lastErr = errors.Join(attemptErrors...)

		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				return "", fmt.Errorf("health check canceled (last error: %v): %w", lastErr, err)
			}
			return "", fmt.Errorf("health check timed out after %s (last error: %v): %w", timeout, lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func checkHealth(ctx context.Context, baseURL string, auth ServerAuth, protocol Protocol) error {
	path := "/global/health"
	if protocol.Normalize() == ProtocolV2 {
		path = "/api/health"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	auth.ApplyFor(req, protocol)
	resp, err := healthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const maxHealthBytes = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBytes+1))
	if err != nil {
		return fmt.Errorf("read %s health: %w", protocol, err)
	}
	if len(body) > maxHealthBytes {
		return fmt.Errorf("decode %s health: response exceeds 64 KiB", protocol)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s health returned status %d", protocol, resp.StatusCode)
	}
	var health struct {
		Healthy bool `json:"healthy"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&health); err != nil {
		return fmt.Errorf("decode %s health: %w", protocol, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s health: multiple JSON values", protocol)
		}
		return fmt.Errorf("decode %s health trailing data: %w", protocol, err)
	}
	if !health.Healthy {
		return fmt.Errorf("%s health reported unhealthy", protocol)
	}
	return nil
}
