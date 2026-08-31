package backgroundopencode

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"unicode/utf8"
)

// ObservePending uses positive process-local evidence only. WorkUnknown does
// not mean idle, complete, successful, or safe to export.
func (c *Client) ObservePending(ctx context.Context, sessionID string) (PendingObservation, error) {
	if !validSessionID(sessionID) {
		return PendingObservation{}, ErrInvalidConfig
	}
	active, err := c.active(ctx)
	if err != nil {
		return PendingObservation{}, err
	}
	questions, err := c.questions(ctx, sessionID)
	if err != nil {
		return PendingObservation{}, err
	}
	permissions, err := c.permissions(ctx, sessionID)
	if err != nil {
		return PendingObservation{}, err
	}
	observation := PendingObservation{State: WorkUnknown, Active: active[sessionID], Questions: len(questions), Permissions: len(permissions)}
	if observation.Active {
		observation.State = WorkWorking
	}
	if observation.Questions > 0 || observation.Permissions > 0 {
		observation.State = WorkNeedsYou
	}
	return observation, nil
}

func (c *Client) active(ctx context.Context) (map[string]bool, error) {
	var envelope struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := c.json(ctx, http.MethodGet, "/api/session/active", nil, http.StatusOK, "read active sessions", statusAuthority{}, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data == nil || len(envelope.Data) > maxPending {
		return nil, protocol("read active sessions", "invalid active set")
	}
	result := make(map[string]bool, len(envelope.Data))
	for id, state := range envelope.Data {
		if !validSessionID(id) || state.Type != "running" {
			return nil, protocol("read active sessions", "invalid active identity or state")
		}
		result[id] = true
	}
	return result, nil
}

func (c *Client) questions(ctx context.Context, sessionID string) ([]question, error) {
	var envelope struct {
		Data []question `json:"data"`
	}
	if err := c.json(ctx, http.MethodGet, sessionPath(sessionID)+"/question", nil, http.StatusOK, "list questions", statusAuthority{sessionID: sessionID}, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data == nil || len(envelope.Data) > maxPending {
		return nil, protocol("list questions", "invalid list bound")
	}
	seen := make(map[string][]byte, len(envelope.Data))
	for _, item := range envelope.Data {
		if !validPrefixedID(item.ID, "que") || item.SessionID != sessionID || item.Questions == nil || len(*item.Questions) > 100 {
			return nil, protocol("list questions", "identity or ownership mismatch")
		}
		wire, _ := json.Marshal(item)
		if prior, ok := seen[item.ID]; ok {
			if !bytes.Equal(prior, wire) {
				return nil, protocol("list questions", "conflicting duplicate identity")
			}
			return nil, protocol("list questions", "duplicate identity")
		}
		seen[item.ID] = wire
		for _, info := range *item.Questions {
			if info.Question == nil || info.Header == nil || info.Options == nil || len(*info.Question) > maxPromptBytes || len(*info.Header) > 1024 || !utf8.ValidString(*info.Question) || !utf8.ValidString(*info.Header) || len(*info.Options) > 100 {
				return nil, protocol("list questions", "invalid question")
			}
			for _, option := range *info.Options {
				if option.Label == nil || option.Description == nil || len(*option.Label) > 4096 || len(*option.Description) > maxPromptBytes || !utf8.ValidString(*option.Label) || !utf8.ValidString(*option.Description) {
					return nil, protocol("list questions", "invalid question option")
				}
			}
		}
	}
	return envelope.Data, nil
}

func (c *Client) permissions(ctx context.Context, sessionID string) ([]permission, error) {
	var envelope struct {
		Data []permission `json:"data"`
	}
	if err := c.json(ctx, http.MethodGet, sessionPath(sessionID)+"/permission", nil, http.StatusOK, "list permissions", statusAuthority{sessionID: sessionID}, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data == nil || len(envelope.Data) > maxPending {
		return nil, protocol("list permissions", "invalid list bound")
	}
	seen := make(map[string][]byte, len(envelope.Data))
	for _, item := range envelope.Data {
		if !validPrefixedID(item.ID, "per") || item.SessionID != sessionID || item.Action == "" || item.Resources == nil || len(item.Resources) > 1000 {
			return nil, protocol("list permissions", "identity or ownership mismatch")
		}
		if item.Source != nil && (item.Source.Type != "tool" || item.Source.MessageID == "" || item.Source.CallID == "") {
			return nil, protocol("list permissions", "invalid source")
		}
		wire, _ := json.Marshal(item)
		if prior, ok := seen[item.ID]; ok {
			if !bytes.Equal(prior, wire) {
				return nil, protocol("list permissions", "conflicting duplicate identity")
			}
			return nil, protocol("list permissions", "duplicate identity")
		}
		seen[item.ID] = wire
	}
	return envelope.Data, nil
}
