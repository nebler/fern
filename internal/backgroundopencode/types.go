// Package backgroundopencode is the narrow HTTP client for Fern's qualified
// source-39fb919a054190498f6d5b7985bde231f93ad7a6 Background Run profile.
// It does not support the persistent workspace OpenCode contract.
package backgroundopencode

import "encoding/json"

const Profile = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"

const (
	maxRequestBytes  = 128 << 10
	maxResponseBytes = 1 << 20
	maxPromptBytes   = 64 << 10
	maxIDBytes       = 256
	maxCredential    = 4096
	maxJSONDepth     = 64
	maxPageLimit     = 100
	maxScanPages     = 1000
	maxScanEvents    = 10000
	maxPending       = 1000
)

type SessionSpec struct {
	ID         string
	Agent      string
	ProviderID string
	ModelID    string
	Directory  string
}

type PromptSpec struct {
	ID       string
	Text     string
	Resume   bool
	Delivery string
}

// ReconcileState is evidence about one intended immutable external identity.
type ReconcileState string

const (
	ReconcileAbsent    ReconcileState = "absent"
	ReconcileAdmitted  ReconcileState = "admitted_not_promoted"
	ReconcileExact     ReconcileState = "exact"
	ReconcileConflict  ReconcileState = "conflict"
	ReconcileUncertain ReconcileState = "uncertain"
)

type HistoryBounds struct {
	PageLimit int
	MaxPages  int
	MaxEvents int
}

type WorkState string

const (
	WorkUnknown  WorkState = "unknown"
	WorkWorking  WorkState = "working"
	WorkNeedsYou WorkState = "needs_you"
)

type PendingObservation struct {
	State       WorkState
	Active      bool
	Questions   int
	Permissions int
}

type modelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

type locationRef struct {
	Directory   string `json:"directory"`
	WorkspaceID string `json:"workspaceID,omitempty"`
}

type sessionInfo struct {
	ID        string         `json:"id"`
	ParentID  string         `json:"parentID,omitempty"`
	ProjectID string         `json:"projectID"`
	Agent     string         `json:"agent,omitempty"`
	Model     *modelRef      `json:"model,omitempty"`
	Cost      *float64       `json:"cost"`
	Tokens    *sessionTokens `json:"tokens"`
	Time      *sessionTime   `json:"time"`
	Title     string         `json:"title"`
	Location  *locationRef   `json:"location"`
	Subpath   string         `json:"subpath,omitempty"`
	Revert    *revertState   `json:"revert,omitempty"`
}

type sessionTokens struct {
	Input     *float64    `json:"input"`
	Output    *float64    `json:"output"`
	Reasoning *float64    `json:"reasoning"`
	Cache     *tokenCache `json:"cache"`
}

type tokenCache struct {
	Read  *float64 `json:"read"`
	Write *float64 `json:"write"`
}

type sessionTime struct {
	Created  *float64 `json:"created"`
	Updated  *float64 `json:"updated"`
	Archived *float64 `json:"archived,omitempty"`
}

type revertState struct {
	MessageID string       `json:"messageID"`
	PartID    string       `json:"partID,omitempty"`
	Snapshot  string       `json:"snapshot,omitempty"`
	Diff      string       `json:"diff,omitempty"`
	Files     []revertFile `json:"files,omitempty"`
}

type revertFile struct {
	Path      string  `json:"path"`
	Status    string  `json:"status"`
	Additions float64 `json:"additions"`
	Deletions float64 `json:"deletions"`
	Patch     string  `json:"patch"`
}

type promptBody struct {
	ID       string     `json:"id"`
	Prompt   promptText `json:"prompt"`
	Delivery string     `json:"delivery"`
	Resume   bool       `json:"resume"`
}

type promptText struct {
	Text   string            `json:"text"`
	Files  []json.RawMessage `json:"files,omitempty"`
	Agents []json.RawMessage `json:"agents,omitempty"`
}

type admission struct {
	AdmittedSeq *int64     `json:"admittedSeq"`
	ID          string     `json:"id"`
	SessionID   string     `json:"sessionID"`
	Prompt      promptText `json:"prompt"`
	Delivery    string     `json:"delivery"`
	TimeCreated *float64   `json:"timeCreated"`
	PromotedSeq *int64     `json:"promotedSeq,omitempty"`
}

type durableEvent struct {
	ID       string                     `json:"id"`
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
	Type     string                     `json:"type"`
	Durable  *durableIdentity           `json:"durable"`
	Location *locationRef               `json:"location,omitempty"`
	Data     json.RawMessage            `json:"data"`
}

type durableIdentity struct {
	AggregateID string `json:"aggregateID"`
	Seq         *int64 `json:"seq"`
	Version     *int64 `json:"version"`
}

type promptEventData struct {
	Timestamp *float64   `json:"timestamp"`
	SessionID string     `json:"sessionID"`
	MessageID string     `json:"messageID"`
	Prompt    promptText `json:"prompt"`
	Delivery  string     `json:"delivery"`
}

type question struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	Questions *[]questionInfo `json:"questions"`
	Tool      *questionTool   `json:"tool,omitempty"`
}

type questionInfo struct {
	Question *string           `json:"question"`
	Header   *string           `json:"header"`
	Options  *[]questionOption `json:"options"`
	Multiple *bool             `json:"multiple,omitempty"`
	Custom   *bool             `json:"custom,omitempty"`
}

type questionOption struct {
	Label       *string `json:"label"`
	Description *string `json:"description"`
}

type questionTool struct {
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}

type permission struct {
	ID        string                     `json:"id"`
	SessionID string                     `json:"sessionID"`
	Action    string                     `json:"action"`
	Resources []string                   `json:"resources"`
	Save      []string                   `json:"save,omitempty"`
	Metadata  map[string]json.RawMessage `json:"metadata,omitempty"`
	Source    *permissionSource          `json:"source,omitempty"`
}

type permissionSource struct {
	Type      string `json:"type"`
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}
