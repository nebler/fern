package runtime

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

func WaitHealthy(ctx context.Context, ep Endpoint, auth ServerAuth, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL()+"/global/health", nil)
		if err != nil {
			return err
		}
		auth.Apply(req)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("health check timed out after %s (last error: %v): %w", timeout, lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}
