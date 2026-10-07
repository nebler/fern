package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nebler/fern/internal/backgroundroute"
)

// Check real pinned-server envelopes, not a mock or a successful status alone.
// The serial run is held working by the fake provider, so both projections must
// contain exactly the attached session; an empty projection is not a success.
func verifySerialAPIProjections(ctx context.Context, origin, token, sessionID string) error {
	for _, path := range []string{"/api/session", "/api/session/active"} {
		status, body, err := serialAttachmentRequest(ctx, origin, token, http.MethodGet, path)
		if err != nil {
			return fmt.Errorf("attached %s projection: %w", path, err)
		}
		if status != http.StatusOK {
			return fmt.Errorf("attached %s projection status=%d", path, status)
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(body, &envelope) != nil || envelope == nil {
			return fmt.Errorf("attached %s projection is not an API envelope", path)
		}
		data, exists := envelope["data"]
		if !exists || string(data) == "null" {
			return fmt.Errorf("attached %s projection lacks non-null data", path)
		}
		if path == "/api/session" {
			var sessions []struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &sessions) != nil || len(sessions) != 1 || sessions[0].ID != sessionID {
				return fmt.Errorf("attached session list must contain exactly the owned session %s: %s", sessionID, data)
			}
		} else {
			var active map[string]struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &active) != nil || len(active) != 1 {
				return fmt.Errorf("attached active-session projection must contain exactly one owned entry for %s: %s", sessionID, data)
			}
			if value, ok := active[sessionID]; !ok || value.Type != "running" {
				return fmt.Errorf("attached active-session projection lacks the owned active entry for %s: %s", sessionID, data)
			}
		}
	}
	return nil
}

func verifySerialDeniedMethods(ctx context.Context, origin, token, sessionID string) error {
	for _, path := range []string{"/api/management/future", "/api/workspace", "/api/provider", "/api/session", "/api/session/" + sessionID + "/future"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			status, _, err := serialAttachmentRequest(ctx, origin, token, method, path)
			if err != nil || status != http.StatusForbidden {
				return fmt.Errorf("attached management write fence method=%s path=%s status=%d error=%v", method, path, status, err)
			}
		}
	}
	for _, path := range []string{
		"/api/session/ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"/api/session/ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/prompt",
		"/api/session/ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/interrupt",
		"/session/ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/message",
		"/api/experimental/session/ses_aaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			status, _, err := serialAttachmentRequest(ctx, origin, token, method, path)
			if err != nil || status != http.StatusForbidden {
				return fmt.Errorf("attached foreign-session fence method=%s path=%s status=%d error=%v", method, path, status, err)
			}
		}
	}
	return nil
}

// Never include response bodies, credentials, or raw transport/decoder errors
// in diagnostics. Bound reads and honor the harness's overall deadline.
func serialAttachmentRequest(ctx context.Context, origin, token, method, path string) (int, []byte, error) {
	var payload io.Reader
	if method != http.MethodGet && method != http.MethodHead {
		payload = strings.NewReader("{}")
	}
	request, err := http.NewRequestWithContext(ctx, method, origin+path, payload)
	if err != nil {
		return 0, nil, errors.New("construct attachment request")
	}
	request.SetBasicAuth(backgroundroute.AttachmentUsername, token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, errors.New("attachment request transport failed")
	}
	defer response.Body.Close()
	const limit = 4 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		return response.StatusCode, nil, errors.New("attachment response unreadable or oversized")
	}
	return response.StatusCode, body, nil
}
