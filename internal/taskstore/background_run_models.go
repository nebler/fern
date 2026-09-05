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
	TaskEventID              task.EventID
	AttemptEventID           task.EventID
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

	BackgroundRunEffectAbsent            = run.Absent
	BackgroundRunEffectProvisionIntent   = run.ProvisionIntent
	BackgroundRunEffectCloneObserved     = run.CloneObserved
	BackgroundRunEffectVolumeObserved    = run.VolumeObserved
	BackgroundRunEffectContainerObserved = run.ContainerObserved
	BackgroundRunEffectHealthObserved    = run.HealthObserved
	BackgroundRunEffectReady             = run.Ready
	BackgroundRunEffectSessionObserved   = run.SessionObserved
	BackgroundRunEffectPromptIntent      = run.PromptIntent
	BackgroundRunEffectPromptAdmitted    = run.PromptAdmitted
	BackgroundRunEffectSealIntent        = run.SealIntent
	BackgroundRunEffectStopIntent        = run.StopIntent
	BackgroundRunEffectWriterInactive    = run.WriterInactive
	BackgroundRunEffectExporting         = run.Exporting
	BackgroundRunEffectArtifactCommitted = run.ArtifactCommitted
	BackgroundRunEffectRouteRemoved      = run.RouteRemoved
	BackgroundRunEffectContainerRemoved  = run.ContainerRemoved
	BackgroundRunEffectVolumeRemoved     = run.VolumeRemoved
	BackgroundRunEffectCloneRemoved      = run.CloneRemoved
	BackgroundRunEffectCleanupComplete   = run.CleanupComplete
	BackgroundRunEffectPreEffectFailed   = run.PreEffectFailed
)

// BackgroundRunIntent is the immutable environment selection committed with a
// task admission. Mutable lifecycle fields live only on BackgroundRun.
type BackgroundRunIntent struct {
	RepositoryRemote, Branch, Profile                   string
	InstructionSHA256, ProfileSHA256, EnvironmentSHA256 [32]byte
	ImageIdentity                                       string
	CloneIdentity, VolumeIdentity, ContainerIdentity    string
	EndpointIdentity                                    string
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
	CancelEpoch                uint64
	StopReceiptID              task.ReceiptID
	StopActor                  *task.ActorSnapshot
	StopRequestedAt            *time.Time
	Creator                    task.ActorSnapshot
	ClaimOwner                 string
	ClaimExpiresAt             *time.Time
	ClaimGeneration            int64
	ObservedContainerID        string
	ObservedContainerStartedAt string
	RuntimeEpoch               int64
	HostPort                   int
	CloneEvidence              string
	VolumeEvidence             string
	HealthEvidence             string
	ReadyEvidence              string
	SessionEvidence            string
	PromptEvidence             string
	WriterInactiveEvidence     string
	RouteRemovedEvidence       string
	ContainerRemovedEvidence   string
	VolumeRemovedEvidence      string
	CloneRemovedEvidence       string
	LastEvidence               string
	LastError                  string
	ProvisionIntentAt          *time.Time
	CloneObservedAt            *time.Time
	VolumeObservedAt           *time.Time
	ContainerObservedAt        *time.Time
	HealthObservedAt           *time.Time
	ReadyAt                    *time.Time
	SessionObservedAt          *time.Time
	PromptIntentAt             *time.Time
	PromptRequestAttemptedAt   *time.Time
	PromptAdmittedAt           *time.Time
	TimeoutRequestedAt         *time.Time
	TimeoutActor               *task.ActorSnapshot
	StopIntentAt               *time.Time
	WriterInactiveAt           *time.Time
	RouteRemovedAt             *time.Time
	ContainerRemovedAt         *time.Time
	VolumeRemovedAt            *time.Time
	CloneRemovedAt             *time.Time
	CleanupCompletedAt         *time.Time
	CleanupProof               string
	AbsenceProof               string
	BackgroundSealRequestID    task.SealRequestID
	ArtifactExportID           task.ArtifactExportID
	RetainedArtifactID         task.RetainedArtifactID
	MaterializationID          task.MaterializationID
	RetainedResultID           task.ResultID
	ResultAuthorityPhase       string
	Revision                   int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

type StopBackgroundRunParams struct {
	WorkspaceID        task.WorkspaceID
	TaskID             task.TaskID
	ReceiptID          task.ReceiptID
	AttemptEventID     task.EventID
	TaskEventID        task.EventID
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
// paired with a claimed run. Prompt is never copied into run evidence.
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

type ClaimNextBackgroundRunParams struct {
	WorkspaceID   task.WorkspaceID
	ClaimOwner    string
	Now           time.Time
	LeaseDuration time.Duration
	Profile       string
	ImageIdentity string
}

type ClaimBackgroundRunParams struct {
	WorkspaceID      task.WorkspaceID
	TaskID           task.TaskID
	AttemptID        task.AttemptID
	Generation       int64
	ExpectedRevision int64
	ExpectedState    BackgroundRunState
	ExpectedPhase    BackgroundRunEffectPhase
	CancelEpoch      uint64
	ClaimOwner       string
	Now              time.Time
	LeaseDuration    time.Duration
	Profile          string
	ImageIdentity    string
}

// BackgroundRunClaim identifies one exact, fenced mutation authority.
type BackgroundRunClaim struct {
	WorkspaceID      task.WorkspaceID
	TaskID           task.TaskID
	AttemptID        task.AttemptID
	Generation       int64
	ClaimOwner       string
	ClaimGeneration  int64
	ExpectedRevision int64
	ExpectedState    BackgroundRunState
	ExpectedPhase    BackgroundRunEffectPhase
	CancelEpoch      uint64
	Now              time.Time
}

type RenewBackgroundRunClaimParams struct {
	BackgroundRunClaim
	LeaseDuration time.Duration
}

type RecordBackgroundRunContainerObservedParams struct {
	BackgroundRunClaim
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	HostPort           int
	Evidence           string
}

type RecordBackgroundRunEvidenceParams struct {
	BackgroundRunClaim
	Evidence string
}

type FinalizeBackgroundRunFailureParams struct {
	BackgroundRunClaim
	AttemptEventID task.EventID
	TaskEventID    task.EventID
	Actor          task.ActorSnapshot
	Reason         string
	Evidence       string
	CleanupProof   string
}

type CompleteBackgroundRunResultCleanupParams struct {
	BackgroundRunClaim
	CleanupProof string
}

type MarkBackgroundRunCleanupRequiredParams struct {
	BackgroundRunClaim
	Error string
}

type RequestBackgroundRunTimeoutParams struct {
	BackgroundRunClaim
	AttemptEventID task.EventID
	TaskEventID    task.EventID
	Actor          task.ActorSnapshot
}

type Admission struct {
	Task         Task
	Attempt      Attempt
	Receipt      Receipt
	TaskEvent    Event
	AttemptEvent Event
	Replayed     bool
}
