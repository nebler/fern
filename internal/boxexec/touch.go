package boxexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// TouchPath is the authenticated endpoint where fern-init sets files' times
// to what they are, which the box's file watchers hear as a change: gVisor
// tells a box only of its own changes, and the daemon passes on those the
// agent box made (package watch).
const TouchPath = "/touch"

// MaxTouch is how many paths one touch request may name.
const MaxTouch = 10_000

// TouchRequest is the body of a touch request.
type TouchRequest struct {
	// Paths are files relative to the workspace.
	Paths []string `json:"paths"`
}

// Touch asks fern-init at addr, authenticating with token, to touch paths,
// MaxTouch at a time. A nil hc means http.DefaultClient.
func Touch(ctx context.Context, hc *http.Client, addr, token string, paths []string) error {
	if hc == nil {
		hc = http.DefaultClient
	}
	for len(paths) > 0 {
		n := min(len(paths), MaxTouch)
		body, err := json.Marshal(TouchRequest{Paths: paths[:n]})
		if err != nil {
			return err
		}
		paths = paths[n:]
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+TouchPath, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("touch: %w", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("touch: %s", resp.Status)
		}
	}
	return nil
}
