package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

var healthHTTPClient = &http.Client{Timeout: 2 * time.Second}

func WaitHealthy(ctx context.Context, ep Endpoint, auth ServerAuth, timeout time.Duration) error {
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL()+"/global/health", nil)
		if err != nil {
			return err
		}
		auth.Apply(req)
		resp, err := healthHTTPClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			closeErr := resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return closeErr
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
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
