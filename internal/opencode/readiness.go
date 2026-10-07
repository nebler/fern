package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

const (
	// The model catalog is the union of every enabled model of every available
	// provider; it is bounded separately because it legitimately exceeds the
	// per-response bound used for session objects.
	maxCatalogBytes   = 8 << 20
	maxCatalogEntries = 20000
	minReadyInterval  = 10 * time.Millisecond
)

// ReadinessSpec names the model and agent a session turn will resolve in one
// OpenCode location.
type ReadinessSpec struct {
	Agent      string
	ProviderID string
	ModelID    string
	Directory  string
}

func validReadinessSpec(spec ReadinessSpec) bool {
	return validToken(spec.Agent, 128) && validToken(spec.ProviderID, 128) && validToken(spec.ModelID, 256) && validLocation(spec.Directory)
}

type catalogLocation struct {
	Directory   string `json:"directory"`
	WorkspaceID string `json:"workspaceID,omitempty"`
	Project     *struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	} `json:"project"`
}

type catalogEnvelope struct {
	Location *catalogLocation  `json:"location"`
	Data     []json.RawMessage `json:"data"`
}

// ModelReady reports whether the location's catalog currently lists the exact
// enabled model and the agent. OpenCode populates both asynchronously after it
// first opens a location, and a turn started before that fails without durable
// evidence, so no prompt may be dispatched until this holds.
func (c *Client) ModelReady(ctx context.Context, spec ReadinessSpec) (bool, error) {
	if !validReadinessSpec(spec) {
		return false, ErrInvalidConfig
	}
	query := "?" + url.Values{"location[directory]": {spec.Directory}}.Encode()
	models, err := c.catalog(ctx, "/api/model"+query, "list models", spec.Directory)
	if err != nil {
		return false, err
	}
	modelFound := false
	for _, raw := range models {
		var model struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Enabled    *bool  `json:"enabled"`
		}
		if err := lenientDecode(raw, &model); err != nil || !validCatalogID(model.ID) || !validCatalogID(model.ProviderID) || model.Enabled == nil {
			return false, protocol("list models", "invalid model entry")
		}
		if model.ProviderID == spec.ProviderID && model.ID == spec.ModelID && *model.Enabled {
			modelFound = true
		}
	}
	if !modelFound {
		return false, nil
	}
	agents, err := c.catalog(ctx, "/api/agent"+query, "list agents", spec.Directory)
	if err != nil {
		return false, err
	}
	for _, raw := range agents {
		var agent struct {
			ID string `json:"id"`
		}
		if err := lenientDecode(raw, &agent); err != nil || !validCatalogID(agent.ID) {
			return false, protocol("list agents", "invalid agent entry")
		}
		if agent.ID == spec.Agent {
			return true, nil
		}
	}
	return false, nil
}

// WaitReady polls ModelReady at interval until it holds or ctx ends. An
// unready catalog at the deadline returns ErrNotReady; any probe failure is
// returned immediately.
func (c *Client) WaitReady(ctx context.Context, spec ReadinessSpec, interval time.Duration) error {
	if interval < minReadyInterval {
		return ErrInvalidConfig
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.Join(ErrNotReady, ctx.Err())
			}
			return ctx.Err()
		case <-timer.C:
		}
		ready, err := c.ModelReady(ctx, spec)
		if ready {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil && (errors.Is(err, ErrTransport) || errors.Is(err, ctx.Err())) {
				continue
			}
			return err
		}
		timer.Reset(interval)
	}
}

func (c *Client) catalog(ctx context.Context, requestPath, operation, directory string) ([]json.RawMessage, error) {
	var envelope catalogEnvelope
	if err := c.jsonBounded(ctx, http.MethodGet, requestPath, nil, http.StatusOK, operation, statusAuthority{}, &envelope, maxCatalogBytes); err != nil {
		return nil, err
	}
	location := envelope.Location
	if location == nil || location.Directory != directory || location.WorkspaceID != "" || location.Project == nil ||
		!validOpaque(location.Project.ID, 512) || !validLocation(location.Project.Directory) {
		return nil, protocol(operation, "location mismatch")
	}
	if envelope.Data == nil || len(envelope.Data) > maxCatalogEntries {
		return nil, protocol(operation, "invalid list bound")
	}
	return envelope.Data, nil
}

// validCatalogID accepts any bounded identity. Catalog entries come from
// models.dev and provider plugins, whose IDs legitimately contain slashes,
// colons, and spaces; only the configured identity must be a token.
func validCatalogID(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value)
}

// lenientDecode reads identity fields from one already strictly checked
// catalog entry. Entries carry evolving provider-specific detail that Fern
// neither needs nor interprets, so unknown fields are permitted here only.
func lenientDecode(raw json.RawMessage, destination any) error {
	if len(raw) == 0 || raw[0] != '{' {
		return ErrProtocol
	}
	return json.NewDecoder(bytes.NewReader(raw)).Decode(destination)
}
