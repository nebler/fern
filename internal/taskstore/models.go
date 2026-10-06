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

// Claim is the idempotency claim this receipt accepted.
func (r Receipt) Claim() task.IdempotencyClaim {
	return task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: r.WorkspaceID, CommandKind: r.CommandKind},
		Key: r.IdempotencyKey, RequestHash: r.RequestHash, Actor: r.Actor}
}

func (r Receipt) classify(incoming task.IdempotencyClaim) task.IdempotencyDisposition {
	claim := r.Claim()
	return task.ClassifyIdempotency(&claim, incoming)
}

type Task struct {
	ID               task.TaskID
	WorkspaceID      task.WorkspaceID
	Title            string
	Prompt           string
	PromptSHA256     [32]byte
	RepositoryID     task.RepositoryID
	BaseRef          string
	BaseSHA          task.GitOID
	ObjectFormat     string
	State            task.TaskState
	TerminalReason   *string
	CurrentAttemptID task.AttemptID
	SealedResultID   task.ResultID
	Revision         int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Attempt struct {
	ID                       task.AttemptID
	TaskID                   task.TaskID
	WorkspaceID              task.WorkspaceID
	Sequence                 int64
	State                    task.AttemptState
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
	PolicyVersion       string
	CollectedAt         time.Time
	SealedAt            time.Time
	CompletionAuthority SealCompletionAuthority
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
	Result   Result
	Manifest []ManifestEntry
	Task     Task
	Attempt  Attempt
	Replayed bool
}
