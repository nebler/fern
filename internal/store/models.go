package store

import (
	"encoding/json"
	"time"

	"github.com/nebler/fern/internal/domain"
)

type WorkspaceState string

type GitHubAuthority string

// WorkspaceActive is the only workspace state; workspaces have no lifecycle.
const WorkspaceActive WorkspaceState = "active"

const (
	GitHubAuthorityAppBroker GitHubAuthority = "github-app-broker"
)

func (authority GitHubAuthority) valid() bool {
	return authority == GitHubAuthorityAppBroker
}

func (s WorkspaceState) valid() bool { return s == WorkspaceActive }

// Workspace is the durable repository and runtime binding needed by task
// admission. It is created once and never transitions.
type Workspace struct {
	ID                  domain.WorkspaceID
	Name                string
	State               WorkspaceState
	RepositoryPath      string
	GitHubAuthority     GitHubAuthority
	InstallationID      domain.InstallationID
	RepositoryID        domain.RepositoryID
	RepositoryFullName  string
	ImageDigest         string
	OpenCodeProtocol    string
	RuntimeDesiredState string
	ReconciliationEpoch uint64
	Revision            int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Receipt is the durable acceptance of one idempotent run command, bound to
// its request hash and authenticated actor.
type Receipt struct {
	ID                 int64
	WorkspaceID        domain.WorkspaceID
	CommandKind        string
	IdempotencyKey     domain.IdempotencyKey
	RequestHash        domain.RequestHash
	Actor              domain.ActorSnapshot
	AcceptedAt         time.Time
	APIContractVersion string
	RunID              domain.RunID
	ResponseStatus     int
	ResponseProjection json.RawMessage
}

// Claim is the idempotency claim this receipt accepted.
func (r Receipt) Claim() domain.IdempotencyClaim {
	return domain.IdempotencyClaim{Scope: domain.IdempotencyScope{WorkspaceID: r.WorkspaceID, CommandKind: r.CommandKind},
		Key: r.IdempotencyKey, RequestHash: r.RequestHash, Actor: r.Actor}
}

func (r Receipt) classify(incoming domain.IdempotencyClaim) domain.IdempotencyDisposition {
	claim := r.Claim()
	return domain.ClassifyIdempotency(&claim, incoming)
}

// AdmitRunParams is the complete immutable intent of one run. The
// store derives the instruction and profile digests and the canonical
// resource identities itself.
type AdmitRunParams struct {
	RunID              domain.RunID
	OpenCodeSessionID  domain.OpenCodeSessionID
	OpenCodeMessageID  domain.OpenCodeMessageID
	Claim              domain.IdempotencyClaim
	Prompt             string
	RepositoryID       domain.RepositoryID
	RepositoryRemote   string
	BaseSHA            domain.GitOID
	Branch             string
	Profile            string
	ImageIdentity      string
	EnvironmentSHA256  [32]byte
	Agent              string
	ModelProvider      string
	Model              string
	Deadline           time.Time
	APIContractVersion string
	AcceptedAt         time.Time
}

// Run is one run row: its immutable intent, lifecycle, and the
// authority recorded once along the way. The prompt itself is read only with
// RunWork.
type Run struct {
	RunID                      domain.RunID
	WorkspaceID                domain.WorkspaceID
	RepositoryID               domain.RepositoryID
	RepositoryRemote           string
	BaseOID                    domain.GitOID
	Branch                     *string
	Agent                      string
	ModelProvider              string
	Model                      string
	Deadline                   time.Time
	Profile                    string
	EnvironmentSHA256          [32]byte
	ResourceSpecVersion        int
	ImageIdentity              string
	CloneIdentity              string
	VolumeIdentity             string
	ContainerIdentity          string
	EndpointIdentity           string
	OpenCodeSessionID          domain.OpenCodeSessionID
	OpenCodeMessageID          domain.OpenCodeMessageID
	Creator                    domain.ActorSnapshot
	State                      domain.State
	EffectPhase                domain.Phase
	StopReceiptID              int64
	StopRequestedAt            *time.Time
	TimeoutRequestedAt         *time.Time
	ObservedContainerID        string
	ObservedContainerStartedAt string
	RuntimeEpoch               int64
	HostPort                   int
	PromptRequestAttemptedAt   *time.Time
	Seal                       *Seal
	WriterFence                *WriterFence
	LastEvidence               string
	LastError                  string
	CleanupProof               string
	Revision                   int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

type StopRunParams struct {
	WorkspaceID        domain.WorkspaceID
	RunID              domain.RunID
	Claim              domain.IdempotencyClaim
	APIContractVersion string
	StoppedAt          time.Time
}

type RunStop struct {
	Run      Run
	Receipt  Receipt
	Replayed bool
}

// RunWork pairs the next runnable run with its plaintext prompt.
// The prompt is never copied into run evidence.
type RunWork struct {
	Run    Run
	Prompt string
}

// RunRef pins one exact run revision. Every coordinator mutation is
// a compare-and-swap on it: fern up's host lease admits one coordinator per
// workspace, and the in-process stop/seal API bumps the same revision, so a
// stale ref fails instead of overwriting a concurrent transition.
type RunRef struct {
	WorkspaceID      domain.WorkspaceID
	RunID            domain.RunID
	ExpectedRevision int64
	ExpectedState    domain.State
	ExpectedPhase    domain.Phase
	Now              time.Time
}

// RecordRunRuntimeParams commits the exact started runtime: the
// identity that later authorizes stop and cleanup.
type RecordRunRuntimeParams struct {
	RunRef
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	HostPort           int
	Evidence           string
}

type RecordRunEvidenceParams struct {
	RunRef
	Evidence string
}

type FinalizeRunFailureParams struct {
	RunRef
	Reason       string
	Evidence     string
	CleanupProof string
}

type CompleteRunResultCleanupParams struct {
	RunRef
	CleanupProof string
}

type MarkRunCleanupRequiredParams struct {
	RunRef
	Error string
}

type Admission struct {
	Run      Run
	Receipt  Receipt
	Replayed bool
}

// Seal is the durable seal admission recorded on its run. Its receipt holds
// the requesting actor, idempotency key, and request hash.
type Seal struct {
	ReceiptID     int64
	ResultID      domain.ResultID
	RequestedAt   time.Time
	PolicyVersion string
}

// CommitEpochSeconds is the normalized Git commit time of the retained
// snapshot, fixed at seal admission so every export pass is deterministic.
func (s Seal) CommitEpochSeconds() int64 { return s.RequestedAt.Unix() }

type SealRunParams struct {
	WorkspaceID         domain.WorkspaceID
	RunID               domain.RunID
	ExpectedRunRevision int64
	ResultID            domain.ResultID
	Claim               domain.IdempotencyClaim
	PolicyVersion       string
	APIContractVersion  string
	AcceptedAt          time.Time
}

type RunSealAdmission struct {
	Run      Run
	Receipt  Receipt
	Replayed bool
}

type WriterFenceKind string

const (
	WriterFenceNeverCreated   WriterFenceKind = "never_created"
	WriterFenceNeverStarted   WriterFenceKind = "never_started"
	WriterFenceRuntimeStopped WriterFenceKind = "runtime_stopped"
)

// WriterFence is the once-recorded proof that a sealed run's exact writer no
// longer runs. Export reads the clone only under it.
type WriterFence struct {
	Kind               WriterFenceKind
	ContainerID        string
	ContainerStartedAt string
	RuntimeToken       string
	StoppedAt          *time.Time
}

type RecordRunWriterFenceParams struct {
	RunRef
	WriterFence
}
