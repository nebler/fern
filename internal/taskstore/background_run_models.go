package taskstore

import (
	"time"

	"github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

type AdmitBackgroundRunParams struct {
	TaskID                   task.TaskID
	AttemptID                task.AttemptID
	ReceiptID                task.ReceiptID
	OpenCodeSessionID        task.OpenCodeSessionID
	OpenCodeMessageID        task.OpenCodeMessageID
	Claim                    task.IdempotencyClaim
	Title                    string
	Prompt                   string
	RepositoryID             task.RepositoryID
	BaseRef                  string
	BaseSHA                  task.GitOID
	ObjectFormat             string
	ExecutionContractVersion string
	Agent                    string
	ModelProvider            string
	Model                    string
	Deadline                 time.Time
	APIContractVersion       string
	AcceptedAt               time.Time
	BackgroundRun            *BackgroundRunIntent
}

// Compatibility names expose the domain vocabulary without maintaining a
// second enum. SQL representation stays in Store.
type BackgroundRunState = run.State
type BackgroundRunEffectPhase = run.Phase

const BackgroundRunSourceProfile = run.SourceProfile

const (
	BackgroundRunQueued          = run.Queued
	BackgroundRunSettingUp       = run.SettingUp
	BackgroundRunWorking         = run.Working
	BackgroundRunNeedsYou        = run.NeedsYou
	BackgroundRunCanceling       = run.Canceling
	BackgroundRunUncertain       = run.Uncertain
	BackgroundRunResultReady     = run.ResultReady
	BackgroundRunFailed          = run.Failed
	BackgroundRunCleanupRequired = run.CleanupRequired

	BackgroundRunEffectAbsent          = run.Absent
	BackgroundRunEffectProvisioning    = run.Provisioning
	BackgroundRunEffectPromptPending   = run.PromptPending
	BackgroundRunEffectAdmitted        = run.Admitted
	BackgroundRunEffectSealing         = run.Sealing
	BackgroundRunEffectCleaning        = run.Cleaning
	BackgroundRunEffectCleanupComplete = run.CleanupComplete
)

// BackgroundRunIntent is the immutable environment selection committed with a
// task admission. The store derives the instruction and profile digests and
// the canonical resource identities itself. Mutable lifecycle fields live only
// on BackgroundRun.
type BackgroundRunIntent struct {
	RepositoryRemote, Branch, Profile string
	EnvironmentSHA256                 [32]byte
	ImageIdentity                     string
}

type BackgroundRun struct {
	TaskID                     task.TaskID
	AttemptID                  task.AttemptID
	WorkspaceID                task.WorkspaceID
	Generation                 int64
	WriterGeneration           int64
	RepositoryID               task.RepositoryID
	RepositoryRemote           string
	BaseOID                    task.GitOID
	Branch                     *string
	InstructionSHA256          [32]byte
	Profile                    string
	ProfileSHA256              [32]byte
	EnvironmentSHA256          [32]byte
	ResourceSpecVersion        int
	ImageIdentity              string
	CloneIdentity              string
	VolumeIdentity             string
	ContainerIdentity          string
	EndpointIdentity           string
	OpenCodeSessionID          task.OpenCodeSessionID
	OpenCodeMessageID          task.OpenCodeMessageID
	State                      BackgroundRunState
	EffectPhase                BackgroundRunEffectPhase
	StopReceiptID              task.ReceiptID
	StopRequestedAt            *time.Time
	Creator                    task.ActorSnapshot
	ObservedContainerID        string
	ObservedContainerStartedAt string
	RuntimeEpoch               int64
	HostPort                   int
	LastEvidence               string
	LastError                  string
	PromptRequestAttemptedAt   *time.Time
	TimeoutRequestedAt         *time.Time
	CleanupProof               string
	Seal                       *Seal
	WriterFence                *WriterFence
	Revision                   int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

type StopBackgroundRunParams struct {
	WorkspaceID        task.WorkspaceID
	TaskID             task.TaskID
	ReceiptID          task.ReceiptID
	Claim              task.IdempotencyClaim
	APIContractVersion string
	StoppedAt          time.Time
}

type BackgroundRunStop struct {
	Run      BackgroundRun
	Receipt  Receipt
	Replayed bool
}

// BackgroundRunWork is the exact task-owned plaintext and attempt deadline
// paired with the next runnable run. Prompt is never copied into run evidence.
type BackgroundRunWork struct {
	Run            BackgroundRun
	Prompt         string
	Deadline       time.Time
	AttemptCreated time.Time
	AttemptTimeout time.Duration
	Agent          string
	ModelProvider  string
	Model          string
}

// BackgroundRunRef pins one exact run revision. Every coordinator mutation is
// a compare-and-swap on it: fern up's host lease admits one coordinator per
// workspace, and the in-process stop/seal API bumps the same revision, so a
// stale ref fails instead of overwriting a concurrent transition.
type BackgroundRunRef struct {
	WorkspaceID      task.WorkspaceID
	TaskID           task.TaskID
	AttemptID        task.AttemptID
	Generation       int64
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
	Task     Task
	Attempt  Attempt
	Receipt  Receipt
	Replayed bool
}
