package taskstore

import (
	"encoding/json"
	"time"

	"github.com/nebler/fern/internal/task"
)

type WorkspaceState string
type GitHubAuthority string

const (
	WorkspaceActive           WorkspaceState = "active"
	WorkspaceMaintenance      WorkspaceState = "maintenance"
	WorkspaceRecoveryRequired WorkspaceState = "recovery_required"
	WorkspaceDisabled         WorkspaceState = "disabled"
)

const (
	GitHubAuthorityAppBroker GitHubAuthority = "github-app-broker"
)

func (authority GitHubAuthority) valid() bool {
	return authority == GitHubAuthorityAppBroker
}

func (s WorkspaceState) valid() bool {
	switch s {
	case WorkspaceActive, WorkspaceMaintenance, WorkspaceRecoveryRequired, WorkspaceDisabled:
		return true
	default:
		return false
	}
}

// Workspace is the durable repository and runtime binding needed by task
// admission. Lifecycle transitions are deliberately outside this tranche.
type Workspace struct {
	ID                  task.WorkspaceID
	Name                string
	State               WorkspaceState
	RepositoryPath      string
	GitHubAuthority     GitHubAuthority
	InstallationID      task.InstallationID
	RepositoryID        task.RepositoryID
	RepositoryFullName  string
	ImageDigest         string
	OpenCodeProtocol    string
	RuntimeDesiredState string
	ReconciliationEpoch uint64
	Revision            int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type Receipt struct {
	ID                 task.ReceiptID
	WorkspaceID        task.WorkspaceID
	CommandKind        string
	State              string
	IdempotencyKey     task.IdempotencyKey
	RequestHash        task.RequestHash
	Actor              task.ActorSnapshot
	AcceptedAt         time.Time
	APIContractVersion string
	TargetType         string
	TargetID           task.TaskID
	ResponseStatus     int
	ResponseProjection json.RawMessage
}

const ReceiptAccepted = "accepted"

// CancellationEffectDisposition is the external work, if any, that a
// coordinator may consider only after the cancellation transaction commits.
type CancellationEffectDisposition string

const (
	CancellationEffectNonePrepared      CancellationEffectDisposition = "none_prepared"
	CancellationEffectReconcileDelivery CancellationEffectDisposition = "reconcile_delivery"
	CancellationEffectInterrupt         CancellationEffectDisposition = "interrupt"
	CancellationEffectNoneTerminal      CancellationEffectDisposition = "none_terminal"
)

func (d CancellationEffectDisposition) valid() bool {
	switch d {
	case CancellationEffectNonePrepared, CancellationEffectReconcileDelivery, CancellationEffectInterrupt, CancellationEffectNoneTerminal:
		return true
	default:
		return false
	}
}

type Task struct {
	ID                         task.TaskID
	WorkspaceID                task.WorkspaceID
	Title                      string
	Prompt                     string
	PromptSHA256               [32]byte
	RepositoryID               task.RepositoryID
	BaseRef                    string
	BaseSHA                    task.GitOID
	ObjectFormat               string
	State                      task.TaskState
	TerminalReason             *string
	CancelEpoch                uint64
	CancellationActor          *task.ActorSnapshot
	CancellationReason         *string
	CancellationRequestedAt    *time.Time
	CancellationReceiptID      task.ReceiptID
	CancellationAttemptID      task.AttemptID
	CancellationAttemptEventID task.EventID
	CancellationTaskEventID    task.EventID
	CancellationEffect         CancellationEffectDisposition
	CurrentAttemptID           task.AttemptID
	SealedResultID             task.ResultID
	Actor                      task.ActorSnapshot
	LatestEventCursor          task.Cursor
	Revision                   int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

type Attempt struct {
	ID                       task.AttemptID
	TaskID                   task.TaskID
	WorkspaceID              task.WorkspaceID
	Sequence                 int64
	State                    task.AttemptState
	DeliveryPhase            DeliveryPhase
	OpenCodeSessionID        task.OpenCodeSessionID
	OpenCodeMessageID        task.OpenCodeMessageID
	PromptSHA256             [32]byte
	BaseSHA                  task.GitOID
	ImageDigest              string
	OpenCodeProtocol         string
	ExecutionContractVersion string
	Agent                    string
	ModelProvider            string
	Model                    string
	Deadline                 time.Time
	DeliveryClaimOwner       *string
	DeliveryClaimExpiresAt   *time.Time
	DeliveryStartedAt        *time.Time
	AdmittedAt               *time.Time
	OpenCodeLogAggregateID   *string
	OpenCodeLogSeq           int64
	CancellationAckAt        *time.Time
	RecoveryReason           *string
	TerminalReason           *string
	SealedResultID           task.ResultID
	Revision                 int64
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

type Result struct {
	ID                  task.ResultID
	TaskID              task.TaskID
	AttemptID           task.AttemptID
	WorkspaceID         task.WorkspaceID
	State               task.ResultState
	Outcome             task.ResultOutcome
	RepositoryID        task.RepositoryID
	BaseSHA             task.GitOID
	ResultCommit        task.GitOID
	TreeOID             task.GitOID
	WorktreeClean       bool
	ManifestEntries     int
	ManifestSHA256      [32]byte
	OpenCodeSessionID   task.OpenCodeSessionID
	OpenCodeMessageID   task.OpenCodeMessageID
	EvidenceSHA256      [32]byte
	PolicyVersion       string
	CollectedAt         time.Time
	SealedAt            time.Time
	Creator             task.ActorSnapshot
	CompletionAuthority SealCompletionAuthority
	SealedEventID       task.EventID
	CompletedEventID    task.EventID
	Revision            int64
	SourceKind          ResultSourceKind
	RetainedArtifactID  task.RetainedArtifactID
	ArtifactExportID    task.ArtifactExportID
	MaterializationID   task.MaterializationID
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type ResultSourceKind string

const (
	ResultSourceRetainedArtifact ResultSourceKind = "retained_artifact"
)

type SealCompletionAuthority string

const (
	SealAuthorityUser SealCompletionAuthority = "user_seal"
)

type ManifestEntry struct {
	PathBase64 string  `json:"pathBase64"`
	ChangeKind string  `json:"changeKind"`
	OldMode    *string `json:"oldMode"`
	NewMode    *string `json:"newMode"`
	OldBlobOID *string `json:"oldBlobOid"`
	NewBlobOID *string `json:"newBlobOid"`
	OldSize    *int64  `json:"oldSize"`
	NewSize    *int64  `json:"newSize"`
}

type SealedResult struct {
	Result      Result
	Manifest    []ManifestEntry
	Task        Task
	Attempt     Attempt
	ResultEvent Event
	TaskEvent   Event
	Replayed    bool
}

type resultMaterial struct {
	ResultID                task.ResultID
	TaskID                  task.TaskID
	AttemptID               task.AttemptID
	ExpectedAttemptRevision int64
	ExpectedTaskRevision    int64
	ResultEventID           task.EventID
	TaskEventID             task.EventID
	RepositoryID            task.RepositoryID
	BaseSHA                 task.GitOID
	ResultCommit            task.GitOID
	TreeOID                 task.GitOID
	Outcome                 task.ResultOutcome
	WorktreeClean           bool
	Manifest                []ManifestEntry
	ManifestSHA256          [32]byte
	OpenCodeSessionID       task.OpenCodeSessionID
	OpenCodeMessageID       task.OpenCodeMessageID
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	PolicyVersion           string
	CollectedAt             time.Time
	SealedAt                time.Time
	Actor                   task.ActorSnapshot
	CompletionAuthority     SealCompletionAuthority
}

// DeliveryPhase is the last durably started delivery effect. It is monotonic:
// reconciliation and cancellation preserve it rather than inferring progress
// from an attempt state.
type DeliveryPhase string

const (
	DeliveryPhaseNone                 DeliveryPhase = "none"
	DeliveryPhaseClaimed              DeliveryPhase = "claimed"
	DeliveryPhaseSessionCreateStarted DeliveryPhase = "session_create_started"
	DeliveryPhaseSessionReady         DeliveryPhase = "session_ready"
	DeliveryPhasePromptStarted        DeliveryPhase = "prompt_started"
)

func (p DeliveryPhase) valid() bool {
	switch p {
	case DeliveryPhaseNone, DeliveryPhaseClaimed, DeliveryPhaseSessionCreateStarted, DeliveryPhaseSessionReady, DeliveryPhasePromptStarted:
		return true
	default:
		return false
	}
}

type Event struct {
	ID          task.EventID
	Cursor      task.Cursor
	WorkspaceID task.WorkspaceID
	TaskID      task.TaskID
	AttemptID   task.AttemptID
	EntityType  string
	EntityID    string
	Type        string
	Version     int
	OccurredAt  time.Time
	Actor       task.ActorSnapshot
	Payload     json.RawMessage
}

type EventPage struct {
	Events     []Event
	NextCursor task.Cursor
	Watermark  task.Cursor
	CaughtUp   bool
}
