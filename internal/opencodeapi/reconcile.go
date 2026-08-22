package opencodeapi

import (
	"context"
	"errors"
	"path"
	"strings"
	"unicode/utf8"
)

// ReconcileSession compares the complete pinned session tuple without returning
// its display fields or raw object to the coordinator.
func (client *Client) ReconcileSession(ctx context.Context, expected CreateSessionRequest) (MatchState, error) {
	if !validCreateSessionRequest(expected) {
		return MatchAbsent, ErrInvalidConfiguration
	}
	session, err := client.ReadSession(ctx, expected.ID)
	if errors.Is(err, ErrNotFound) {
		return MatchAbsent, nil
	}
	if err != nil {
		return MatchAbsent, err
	}
	if !sessionMatchesRequest(session, expected) {
		return MatchConflict, nil
	}
	return MatchExact, nil
}

// ReconcilePrompt performs exact, read-only reconciliation for one persisted
// prompt identity. It compares semantic fields in memory and returns no content.
func (client *Client) ReconcilePrompt(ctx context.Context, sessionID string, expected PromptRequest) (PromptObservation, error) {
	observation := PromptObservation{Session: MatchAbsent, Inbox: MatchAbsent, Message: MatchAbsent, Resume: MatchAbsent}
	if !validID(sessionID, "ses") || !validID(expected.ID, "msg_") || expected.Resume == nil {
		return observation, ErrInvalidConfiguration
	}
	if len(expected.Text) > MaxPromptTextBytes {
		return observation, ErrRequestTooLarge
	}
	if expected.Text == "" || !utf8.ValidString(expected.Text) {
		return observation, ErrInvalidConfiguration
	}

	if _, err := client.ReadSession(ctx, sessionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return observation, nil
		}
		return observation, err
	}
	observation.Session = MatchExact

	inbox, err := client.ListInbox(ctx, sessionID)
	if err != nil {
		return observation, err
	}
	for _, item := range inbox {
		if item.ID != expected.ID {
			continue
		}
		observation.Inbox = MatchExact
		observation.Resume = MatchUnobservable
		if item.SessionID != sessionID || item.Type != "user" || item.Delivery != "steer" ||
			item.Payload.Text != expected.Text || item.TimeCreated <= 0 {
			observation.Inbox = MatchConflict
		}
		break
	}

	message, err := client.ReadMessage(ctx, sessionID, expected.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return observation, err
	}
	if err == nil {
		observation.Message = MatchExact
		observation.Resume = MatchUnobservable
		if message.Type != "user" || message.Text != expected.Text || message.Time.Created <= 0 || !*expected.Resume {
			observation.Message = MatchConflict
		}
	}

	if observation.Inbox != MatchAbsent && observation.Message != MatchAbsent {
		return observation, protocolError("prompt identity exists in inbox and message history")
	}
	return observation, nil
}

func validCreateSessionRequest(request CreateSessionRequest) bool {
	return validID(request.ID, "ses") && validUTF8Text(request.Title, 1, 200) &&
		validASCIIValue(request.Agent, 1, 128) && request.Model != nil &&
		validASCIIValue(request.Model.ProviderID, 1, 128) && validASCIIValue(request.Model.ID, 1, 256) &&
		request.Location != nil && validSessionDirectory(request.Location.Directory)
}

func sessionMatchesRequest(session Session, request CreateSessionRequest) bool {
	return session.ID == request.ID && session.Title == request.Title && session.Agent == request.Agent &&
		session.Model != nil && request.Model != nil && session.Model.ProviderID == request.Model.ProviderID && session.Model.ID == request.Model.ID &&
		session.Location != nil && request.Location != nil && session.Location.Directory == request.Location.Directory
}

func validUTF8Text(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validASCIIValue(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, char := range []byte(value) {
		if char <= 0x20 || char >= 0x7f {
			return false
		}
	}
	return true
}

func validSessionDirectory(directory string) bool {
	return len(directory) <= 4096 && strings.HasPrefix(directory, "/") && path.Clean(directory) == directory &&
		validUTF8Text(directory, 2, 4096)
}
