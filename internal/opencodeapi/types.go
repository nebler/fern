package opencodeapi

import (
	"bytes"
	"encoding/json"
)

const MaxPromptTextBytes = 64 << 10

type Model struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
}

type Location struct {
	Directory string `json:"directory"`
}

// CreateSessionRequest is the caller-selected session shape observed for the
// pinned profile. Reusing ID asks OpenCode to adopt the existing session.
type CreateSessionRequest struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Model    *Model    `json:"model,omitempty"`
	Location *Location `json:"location,omitempty"`
}

type PromptRequest struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Resume *bool  `json:"resume,omitempty"`
}

type FormReplyRequest struct {
	Answer json.RawMessage `json:"answer"`
}

type Permission struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionID"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Agent     string   `json:"agent"`
	raw       json.RawMessage
}

func (value *Permission) UnmarshalJSON(data []byte) error {
	type wire Permission
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = Permission(decoded)
	value.raw = append(value.raw[:0], data...)
	return nil
}

// Bytes returns a copy of the bounded wire object for semantic reconciliation.
func (value Permission) Bytes() []byte { return bytes.Clone(value.raw) }

// Session contains the stable identity plus the bounded complete wire object.
// The harness does not establish a closed schema for session display fields.
type Session struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Agent    string    `json:"agent"`
	Model    *Model    `json:"model"`
	Location *Location `json:"location"`
	raw      json.RawMessage
}

func (value *Session) UnmarshalJSON(data []byte) error {
	type wire Session
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = Session(decoded)
	value.raw = append(value.raw[:0], data...)
	return nil
}

func (value Session) Bytes() []byte { return bytes.Clone(value.raw) }

type Admission struct {
	ID  string `json:"id"`
	raw json.RawMessage
}

func (value *Admission) UnmarshalJSON(data []byte) error {
	type wire Admission
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = Admission(decoded)
	value.raw = append(value.raw[:0], data...)
	return nil
}

func (value Admission) Bytes() []byte { return bytes.Clone(value.raw) }

type InboxItem struct {
	ID          string       `json:"id"`
	SessionID   string       `json:"sessionID"`
	Type        string       `json:"type"`
	Delivery    string       `json:"delivery"`
	Payload     InboxPayload `json:"payload"`
	TimeCreated int64        `json:"timeCreated"`
	raw         json.RawMessage
}

type InboxPayload struct {
	Text string `json:"text"`
}

func (value *InboxItem) UnmarshalJSON(data []byte) error {
	type wire InboxItem
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = InboxItem(decoded)
	value.raw = append(value.raw[:0], data...)
	return nil
}

func (value InboxItem) Bytes() []byte { return bytes.Clone(value.raw) }

type MessageTime struct {
	Created int64 `json:"created"`
}

type Message struct {
	ID   string      `json:"id"`
	Type string      `json:"type"`
	Text string      `json:"text,omitempty"`
	Time MessageTime `json:"time"`
	raw  json.RawMessage
}

func (value *Message) UnmarshalJSON(data []byte) error {
	type wire Message
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = Message(decoded)
	value.raw = append(value.raw[:0], data...)
	return nil
}

func (value Message) Bytes() []byte { return bytes.Clone(value.raw) }

type MatchState string

const (
	MatchAbsent       MatchState = "absent"
	MatchExact        MatchState = "exact"
	MatchConflict     MatchState = "conflict"
	MatchUnobservable MatchState = "unobservable"
)

// PromptObservation contains only closed reconciliation states. It never
// returns prompt text or raw OpenCode objects.
type PromptObservation struct {
	Session MatchState
	Inbox   MatchState
	Message MatchState
	Resume  MatchState
}

func (o PromptObservation) Admitted() bool {
	return o.Session == MatchExact && o.Resume == MatchUnobservable &&
		((o.Inbox == MatchExact && o.Message == MatchAbsent) ||
			(o.Inbox == MatchAbsent && o.Message == MatchExact))
}

type MessagePage struct {
	Data       []Message
	NextCursor *string
}

type ActiveSession struct {
	Type string `json:"type"`
}

// ActiveSessions is keyed by exact session ID.
type ActiveSessions map[string]ActiveSession

type FormMetadata struct {
	Kind string `json:"kind"`
}

type FormOption struct {
	Label string `json:"label"`
	Value string `json:"value,omitempty"`
}

type FormField struct {
	Key     string       `json:"key,omitempty"`
	Label   string       `json:"label,omitempty"`
	Options []FormOption `json:"options,omitempty"`
}

type Form struct {
	ID        string       `json:"id"`
	SessionID string       `json:"sessionID"`
	Metadata  FormMetadata `json:"metadata"`
	Fields    []FormField  `json:"fields"`
	raw       json.RawMessage
}

func (value *Form) UnmarshalJSON(data []byte) error {
	type wire Form
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = Form(decoded)
	value.raw = append(value.raw[:0], data...)
	return nil
}

func (value Form) Bytes() []byte { return bytes.Clone(value.raw) }

type FormState struct {
	Status string          `json:"status"`
	Answer json.RawMessage `json:"answer,omitempty"`
	raw    json.RawMessage
}

func (value *FormState) UnmarshalJSON(data []byte) error {
	type wire FormState
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*value = FormState(decoded)
	value.raw = append(value.raw[:0], data...)
	return nil
}

func (value FormState) Bytes() []byte { return bytes.Clone(value.raw) }

// Context is the bounded complete context projection. Its inner schema is not
// closed by the pinned harness, so callers must interpret Bytes by profile.
type Context struct {
	raw json.RawMessage
}

func (value *Context) UnmarshalJSON(data []byte) error {
	if !json.Valid(data) || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ErrInvalidResponse
	}
	value.raw = append(value.raw[:0], data...)
	return nil
}

func (value Context) Bytes() []byte { return bytes.Clone(value.raw) }
