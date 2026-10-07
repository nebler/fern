package taskstore

import (
	"encoding/json"
	"time"

	rundomain "github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
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

// Receipt is the durable acceptance of one idempotent run command, bound to
// its request hash and authenticated actor.
type Receipt struct {
	ID                 int64
	WorkspaceID        task.WorkspaceID
	CommandKind        string
	IdempotencyKey     task.IdempotencyKey
	RequestHash        task.RequestHash
	Actor              task.ActorSnapshot
	AcceptedAt         time.Time
	APIContractVersion string
	RunID              task.RunID
	ResponseStatus     int
	ResponseProjection json.RawMessage
}

// Claim is the idempotency claim this receipt accepted.
func (r Receipt) Claim() task.IdempotencyClaim {
	return task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: r.WorkspaceID, CommandKind: r.CommandKind},
		Key: r.IdempotencyKey, RequestHash: r.RequestHash, Actor: r.Actor}
}

func (r Receipt) classify(incoming task.IdempotencyClaim) task.IdempotencyDisposition {
	claim := r.Claim()
	return task.ClassifyIdempotency(&claim, incoming)
}

// AdmitBackgroundRunParams is the complete immutable intent of one run. The
// store derives the instruction and profile digests and the canonical
// resource identities itself.
type AdmitBackgroundRunParams struct {
	RunID              task.RunID
	OpenCodeSessionID  task.OpenCodeSessionID
	OpenCodeMessageID  task.OpenCodeMessageID
	Claim              task.IdempotencyClaim
	Prompt             string
	RepositoryID       task.RepositoryID
	RepositoryRemote   string
	BaseSHA            task.GitOID
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

// Compatibility names for packages outside taskstore that still spell the run
// vocabulary through this package; taskstore and runapi use package run
// directly. Remove once those callers do too.
type BackgroundRunState = rundomain.State

type BackgroundRunEffectPhase = rundomain.Phase

const BackgroundRunSourceProfile = rundomain.SourceProfile

const (
	BackgroundRunQueued          = rundomain.Queued
	BackgroundRunSettingUp       = rundomain.SettingUp
	BackgroundRunWorking         = rundomain.Working
	BackgroundRunNeedsYou        = rundomain.NeedsYou
	BackgroundRunCanceling       = rundomain.Canceling
	BackgroundRunUncertain       = rundomain.Uncertain
	BackgroundRunResultReady     = rundomain.ResultReady
	BackgroundRunFailed          = rundomain.Failed
	BackgroundRunCleanupRequired = rundomain.CleanupRequired

	BackgroundRunEffectAbsent          = rundomain.Absent
	BackgroundRunEffectProvisioning    = rundomain.Provisioning
	BackgroundRunEffectPromptPending   = rundomain.PromptPending
	BackgroundRunEffectAdmitted        = rundomain.Admitted
	BackgroundRunEffectSealing         = rundomain.Sealing
	BackgroundRunEffectCleaning        = rundomain.Cleaning
	BackgroundRunEffectCleanupComplete = rundomain.CleanupComplete
)

// BackgroundRun is one run row: its immutable intent, lifecycle, and the
// authority recorded once along the way. The prompt itself is read only with
// BackgroundRunWork.
type BackgroundRun struct {
	RunID                      task.RunID
	WorkspaceID                task.WorkspaceID
	RepositoryID               task.RepositoryID
	RepositoryRemote           string
	BaseOID                    task.GitOID
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
	OpenCodeSessionID          task.OpenCodeSessionID
	OpenCodeMessageID          task.OpenCodeMessageID
	Creator                    task.ActorSnapshot
	State                      BackgroundRunState
	EffectPhase                BackgroundRunEffectPhase
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

type StopBackgroundRunParams struct {
	WorkspaceID        task.WorkspaceID
	RunID              task.RunID
	Claim              task.IdempotencyClaim
	APIContractVersion string
	StoppedAt          time.Time
}

type BackgroundRunStop struct {
	Run      BackgroundRun
	Receipt  Receipt
	Replayed bool
}

// BackgroundRunWork pairs the next runnable run with its plaintext prompt.
// The prompt is never copied into run evidence.
type BackgroundRunWork struct {
	Run    BackgroundRun
	Prompt string
}

// BackgroundRunRef pins one exact run revision. Every coordinator mutation is
// a compare-and-swap on it: fern up's host lease admits one coordinator per
// workspace, and the in-process stop/seal API bumps the same revision, so a
// stale ref fails instead of overwriting a concurrent transition.
type BackgroundRunRef struct {
	WorkspaceID      task.WorkspaceID
	RunID            task.RunID
	ExpectedRevision int64
	ExpectedState    BackgroundRunState
	ExpectedPhase    BackgroundRunEffectPhase
	Now              time.Time
}

// RecordBackgroundRunRuntimeParams commits the exact started runtime: the
// identity that later authorizes stop and cleanup.
type RecordBackgroundRunRuntimeParams struct {
	BackgroundRunRef
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	HostPort           int
	Evidence           string
}

type RecordBackgroundRunEvidenceParams struct {
	BackgroundRunRef
	Evidence string
}

type FinalizeBackgroundRunFailureParams struct {
	BackgroundRunRef
	Reason       string
	Evidence     string
	CleanupProof string
}

type CompleteBackgroundRunResultCleanupParams struct {
	BackgroundRunRef
	CleanupProof string
}

type MarkBackgroundRunCleanupRequiredParams struct {
	BackgroundRunRef
	Error string
}

type Admission struct {
	Run      BackgroundRun
	Receipt  Receipt
	Replayed bool
}

// Seal is the durable seal admission recorded on its run. Its receipt holds
// the requesting actor, idempotency key, and request hash.
type Seal struct {
	ReceiptID     int64
	ResultID      task.ResultID
	RequestedAt   time.Time
	PolicyVersion string
}

// CommitEpochSeconds is the normalized Git commit time of the retained
// snapshot, fixed at seal admission so every export pass is deterministic.
func (s Seal) CommitEpochSeconds() int64 { return s.RequestedAt.Unix() }

type SealBackgroundRunParams struct {
	WorkspaceID         task.WorkspaceID
	RunID               task.RunID
	ExpectedRunRevision int64
	ResultID            task.ResultID
	Claim               task.IdempotencyClaim
	PolicyVersion       string
	APIContractVersion  string
	AcceptedAt          time.Time
}

type BackgroundRunSealAdmission struct {
	Run      BackgroundRun
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

type RecordBackgroundRunWriterFenceParams struct {
	BackgroundRunRef
	WriterFence
}
