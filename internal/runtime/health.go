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

const maxHealthBytes = 64 << 10

var (
	errUnsafeHealthAuth = errors.New("backend authentication is not enforced")
	healthHTTPClient    = &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

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
			if errors.Is(err, errUnsafeHealthAuth) {
				return err
			}
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
	if auth.Password != "" {
		if err := checkHealthAuthRejection(ctx, baseURL, ServerAuth{}, "missing"); err != nil {
			return err
		}

		wrongPassword := "fern-health-check-invalid-password"
		if wrongPassword == auth.Password {
			wrongPassword += "-different"
		}
		if err := checkHealthAuthRejection(ctx, baseURL, ServerAuth{Password: wrongPassword}, "invalid"); err != nil {
			return err
		}
	}

	status, body, err := requestHealth(ctx, baseURL, auth)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("health returned status %d", status)
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

func checkHealthAuthRejection(ctx context.Context, baseURL string, auth ServerAuth, probe string) error {
	status, _, err := requestHealth(ctx, baseURL, auth)
	if status >= 200 && status < 300 {
		return fmt.Errorf("%w: %s-credential probe returned status %d", errUnsafeHealthAuth, probe, status)
	}
	if err != nil {
		return fmt.Errorf("%s-credential probe failed: %w", probe, err)
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("%s-credential probe returned status %d", probe, status)
	}
	return nil
}

func requestHealth(ctx context.Context, baseURL string, auth ServerAuth) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/health", nil)
	if err != nil {
		return 0, nil, err
	}
	auth.Apply(req)
	resp, err := healthHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBytes+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read health: %w", err)
	}
	if len(body) > maxHealthBytes {
		return resp.StatusCode, nil, fmt.Errorf("decode health: response exceeds 64 KiB")
	}
	return resp.StatusCode, body, nil
}
