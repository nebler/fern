package taskstore

import (
	"time"

	"github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
)

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
