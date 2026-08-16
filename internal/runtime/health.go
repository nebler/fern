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
	return WaitHealthyURL(ctx, ep.URL(), auth, timeout)
}

func WaitHealthyURL(ctx context.Context, baseURL string, auth ServerAuth, timeout time.Duration) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		if err := checkHealth(ctx, baseURL, auth); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			if err := parent.Err(); err != nil {
				return fmt.Errorf("health check canceled (last error: %v): %w", lastErr, err)
			}
			return fmt.Errorf("health check timed out after %s (last error: %v): %w", timeout, lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func checkHealth(ctx context.Context, baseURL string, auth ServerAuth) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/health", nil)
	if err != nil {
		return err
	}
	auth.Apply(req)
	resp, err := healthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const maxHealthBytes = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBytes+1))
	if err != nil {
		return fmt.Errorf("read health: %w", err)
	}
	if len(body) > maxHealthBytes {
		return fmt.Errorf("decode health: response exceeds 64 KiB")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned status %d", resp.StatusCode)
	}
	var health struct {
		Healthy bool `json:"healthy"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&health); err != nil {
		return fmt.Errorf("decode health: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode health: multiple JSON values")
		}
		return fmt.Errorf("decode health trailing data: %w", err)
	}
	if !health.Healthy {
		return fmt.Errorf("health reported unhealthy")
	}
	return nil
}
