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
	GitHubAuthorityWorkspaceGH GitHubAuthority = "workspace-gh"
	GitHubAuthorityAppBroker   GitHubAuthority = "github-app-broker"
)

func (authority GitHubAuthority) valid() bool {
	return authority == GitHubAuthorityWorkspaceGH || authority == GitHubAuthorityAppBroker
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
	BudgetSnapshot           json.RawMessage
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
	SealRequestID       task.SealRequestID
	Authorizer          *task.ActorSnapshot
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
	ResultSourcePersistentWorkspace ResultSourceKind = "persistent_workspace"
	ResultSourceRetainedArtifact    ResultSourceKind = "retained_artifact"
)

type SealCompletionAuthority string

const (
	SealAuthorityExecutionSuccess SealCompletionAuthority = "execution_success"
	SealAuthorityUser             SealCompletionAuthority = "user_seal"
)

type SealRequestState string

const (
	SealRequestPending   SealRequestState = "pending"
	SealRequestClaimed   SealRequestState = "claimed"
	SealRequestCompleted SealRequestState = "completed"
	SealRequestRejected  SealRequestState = "rejected"
)

// SealPreview is the exact mutable ownership snapshot an HTTP layer presents
// before asking a user to authorize a committed repository snapshot.
type SealPreview struct {
	Workspace Workspace
	Task      Task
	Attempt   Attempt
}

// SealRequest is immutable authorization plus coordinator lease/completion
// state. Expected* fields are the values the user actually approved.
type SealRequest struct {
	ID                        task.SealRequestID
	ReceiptID                 task.ReceiptID
	WorkspaceID               task.WorkspaceID
	TaskID                    task.TaskID
	AttemptID                 task.AttemptID
	State                     SealRequestState
	CompletionAuthority       SealCompletionAuthority
	ExpectedWorkspaceRevision int64
	ExpectedTaskRevision      int64
	ExpectedAttemptRevision   int64
	RepositoryID              task.RepositoryID
	BaseSHA                   task.GitOID
	ExpectedResultCommit      task.GitOID
	ExpectedTreeOID           task.GitOID
	ExpectedOutcome           task.ResultOutcome
	ExpectedManifestEntries   int
	ExpectedManifestSHA256    [32]byte
	ExpectedWorktreeClean     bool
	IdempotencyKey            task.IdempotencyKey
	RequestHash               task.RequestHash
	Authorizer                task.ActorSnapshot
	ResultID                  task.ResultID
	ResultEventID             task.EventID
	TaskEventID               task.EventID
	ClaimOwner                string
	ClaimExpiresAt            *time.Time
	ClaimRevision             int64
	AcceptedAt                time.Time
	CompletedAt               *time.Time
	RejectedAt                *time.Time
	RejectedReason            string
}

type SealAdmission struct {
	Request  SealRequest
	Receipt  Receipt
	Preview  SealPreview
	Replayed bool
}

type RequestSealParams struct {
	SealRequestID             task.SealRequestID
	ReceiptID                 task.ReceiptID
	ResultID                  task.ResultID
	ResultEventID             task.EventID
	TaskEventID               task.EventID
	TaskID                    task.TaskID
	Claim                     task.IdempotencyClaim
	ExpectedWorkspaceRevision int64
	ExpectedTaskRevision      int64
	ExpectedAttemptRevision   int64
	RepositoryID              task.RepositoryID
	BaseSHA                   task.GitOID
	ExpectedResultCommit      task.GitOID
	ExpectedTreeOID           task.GitOID
	ExpectedOutcome           task.ResultOutcome
	ExpectedManifestEntries   int
	ExpectedManifestSHA256    [32]byte
	ExpectedWorktreeClean     bool
	APIContractVersion        string
	AcceptedAt                time.Time
}

type ClaimSealRequestParams struct {
	WorkspaceID    task.WorkspaceID
	ClaimOwner     string
	Now            time.Time
	LeaseExpiresAt time.Time
}

type SealRequestWork struct {
	Request SealRequest
	Preview SealPreview
}

type RejectSealRequestParams struct {
	SealRequestID         task.SealRequestID
	ClaimOwner            string
	ExpectedClaimRevision int64
	Reason                string
	RejectedAt            time.Time
}

type SealAuthorizedResultParams struct {
	SealRequestID         task.SealRequestID
	ClaimOwner            string
	ExpectedClaimRevision int64
	Result                SealResultParams
}

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

// VerificationSource is the exact current ownership tuple returned by
// verification discovery. Discovery grants no authority to execute a command;
// every write rechecks these revisions and identities transactionally.
type VerificationSource struct {
	Result  Result
	Task    Task
	Attempt Attempt
}

type ExecutionProjectionOutcome string

const (
	ExecutionRunning          ExecutionProjectionOutcome = "running"
	ExecutionInputRequired    ExecutionProjectionOutcome = "input_required"
	ExecutionRecoveryRequired ExecutionProjectionOutcome = "recovery_required"
	ExecutionFailed           ExecutionProjectionOutcome = "failed"
	ExecutionSucceeded        ExecutionProjectionOutcome = "succeeded"
)

type RecordExecutionProjectionParams struct {
	TaskID                  task.TaskID
	AttemptID               task.AttemptID
	ExpectedAttemptRevision int64
	ExpectedTaskRevision    int64
	ExpectedState           task.AttemptState
	OpenCodeSessionID       task.OpenCodeSessionID
	OpenCodeMessageID       task.OpenCodeMessageID
	Outcome                 ExecutionProjectionOutcome
	Reason                  string
	AttemptEventID          task.EventID
	TaskEventID             task.EventID
	ObservedAt              time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type ExecutionProjection struct {
	Task         Task
	Attempt      Attempt
	AttemptEvent Event
	TaskEvent    Event
	Replayed     bool
}

type SealResultParams struct {
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
	SealRequestID           task.SealRequestID
	Authorizer              *task.ActorSnapshot
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

type AdmitTaskParams struct {
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
	BudgetSnapshot           json.RawMessage
	Deadline                 time.Time
	APIContractVersion       string
	AcceptedAt               time.Time
	BackgroundRun            *BackgroundRunIntent
}

type BackgroundRunState string
type BackgroundRunEffectPhase string

const BackgroundRunSourceProfile = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"

const (
	BackgroundRunQueued          BackgroundRunState = "queued"
	BackgroundRunSettingUp       BackgroundRunState = "setting_up"
	BackgroundRunWorking         BackgroundRunState = "working"
	BackgroundRunNeedsYou        BackgroundRunState = "needs_you"
	BackgroundRunCanceling       BackgroundRunState = "canceling"
	BackgroundRunUncertain       BackgroundRunState = "uncertain"
	BackgroundRunResultReady     BackgroundRunState = "result_ready"
	BackgroundRunFailed          BackgroundRunState = "failed"
	BackgroundRunCleanupRequired BackgroundRunState = "cleanup_required"

	BackgroundRunEffectAbsent                 BackgroundRunEffectPhase = "absent"
	BackgroundRunEffectProvisionIntent        BackgroundRunEffectPhase = "provision_intent"
	BackgroundRunEffectCloneObserved          BackgroundRunEffectPhase = "clone_observed"
	BackgroundRunEffectVolumeObserved         BackgroundRunEffectPhase = "volume_observed"
	BackgroundRunEffectContainerObserved      BackgroundRunEffectPhase = "container_observed"
	BackgroundRunEffectHealthObserved         BackgroundRunEffectPhase = "health_observed"
	BackgroundRunEffectReady                  BackgroundRunEffectPhase = "ready"
	BackgroundRunEffectSessionObserved        BackgroundRunEffectPhase = "session_observed"
	BackgroundRunEffectPromptIntent           BackgroundRunEffectPhase = "prompt_intent"
	BackgroundRunEffectPromptAdmitted         BackgroundRunEffectPhase = "prompt_admitted"
	BackgroundRunEffectSealIntent             BackgroundRunEffectPhase = "seal_intent"
	BackgroundRunEffectStopIntent             BackgroundRunEffectPhase = "stop_intent"
	BackgroundRunEffectWriterInactive         BackgroundRunEffectPhase = "writer_inactive"
	BackgroundRunEffectExporting              BackgroundRunEffectPhase = "exporting"
	BackgroundRunEffectArtifactCommitted      BackgroundRunEffectPhase = "artifact_committed"
	BackgroundRunEffectRouteRemoved           BackgroundRunEffectPhase = "route_removed"
	BackgroundRunEffectContainerRemoved       BackgroundRunEffectPhase = "container_removed"
	BackgroundRunEffectVolumeRemoved          BackgroundRunEffectPhase = "volume_removed"
	BackgroundRunEffectCloneRemoved           BackgroundRunEffectPhase = "clone_removed"
	BackgroundRunEffectCleanupComplete        BackgroundRunEffectPhase = "cleanup_complete"
	BackgroundRunEffectPreEffectFailed        BackgroundRunEffectPhase = "pre_effect_failed"
	backgroundRunEffectLegacyProvisionStarted BackgroundRunEffectPhase = "provision_started"
	backgroundRunEffectLegacyPromptStarted    BackgroundRunEffectPhase = "prompt_started"
	backgroundRunEffectLegacyStopStarted      BackgroundRunEffectPhase = "stop_started"
	backgroundRunEffectLegacyExportStarted    BackgroundRunEffectPhase = "export_started"
	backgroundRunEffectLegacyCleanupStarted   BackgroundRunEffectPhase = "cleanup_started"
)

func (state BackgroundRunState) valid() bool {
	switch state {
	case BackgroundRunQueued, BackgroundRunSettingUp, BackgroundRunWorking, BackgroundRunNeedsYou,
		BackgroundRunCanceling, BackgroundRunUncertain, BackgroundRunResultReady, BackgroundRunFailed,
		BackgroundRunCleanupRequired:
		return true
	default:
		return false
	}
}

func (phase BackgroundRunEffectPhase) valid() bool {
	switch phase {
	case BackgroundRunEffectAbsent, BackgroundRunEffectProvisionIntent, BackgroundRunEffectCloneObserved,
		BackgroundRunEffectVolumeObserved, BackgroundRunEffectContainerObserved, BackgroundRunEffectHealthObserved,
		BackgroundRunEffectReady, BackgroundRunEffectSessionObserved, BackgroundRunEffectPromptIntent,
		BackgroundRunEffectPromptAdmitted, BackgroundRunEffectSealIntent, BackgroundRunEffectStopIntent, BackgroundRunEffectWriterInactive,
		BackgroundRunEffectExporting, BackgroundRunEffectArtifactCommitted,
		BackgroundRunEffectRouteRemoved, BackgroundRunEffectContainerRemoved, BackgroundRunEffectVolumeRemoved,
		BackgroundRunEffectCloneRemoved, BackgroundRunEffectCleanupComplete, BackgroundRunEffectPreEffectFailed:
		return true
	case backgroundRunEffectLegacyProvisionStarted, backgroundRunEffectLegacyPromptStarted,
		backgroundRunEffectLegacyStopStarted, backgroundRunEffectLegacyExportStarted, backgroundRunEffectLegacyCleanupStarted:
		return true
	default:
		return false
	}
}

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

type OpenBackgroundRunParams struct {
	WorkspaceID        task.WorkspaceID
	TaskID             task.TaskID
	ReceiptID          task.ReceiptID
	Claim              task.IdempotencyClaim
	URL                string
	APIContractVersion string
	OpenedAt           time.Time
}

type BackgroundRunOpen struct {
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

type RequestCancellationParams struct {
	TaskID             task.TaskID
	ReceiptID          task.ReceiptID
	AttemptEventID     task.EventID
	TaskEventID        task.EventID
	Claim              task.IdempotencyClaim
	Reason             string
	Now                time.Time
	APIContractVersion string
}

type Cancellation struct {
	Task         Task
	Attempt      Attempt
	Receipt      Receipt
	AttemptEvent Event
	TaskEvent    Event
	Disposition  CancellationEffectDisposition
	Replayed     bool
}

// AcknowledgeCancellationParams is the coordinator's proof that the one
// persisted cancellation effect has reached a safe, closed outcome.
type AcknowledgeCancellationParams struct {
	TaskID                  task.TaskID
	AttemptID               task.AttemptID
	CancelEpoch             uint64
	ExpectedAttemptRevision int64
	ExpectedTaskRevision    int64
	AttemptEventID          task.EventID
	TaskEventID             task.EventID
	Now                     time.Time
	Disposition             CancellationEffectDisposition
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type CancellationAcknowledgment struct {
	Task         Task
	Attempt      Attempt
	AttemptEvent Event
	TaskEvent    Event
	Disposition  CancellationEffectDisposition
	Replayed     bool
}

type EventPage struct {
	Events     []Event
	NextCursor task.Cursor
	Watermark  task.Cursor
	CaughtUp   bool
}

type DeliveryWork struct {
	Task    Task
	Attempt Attempt
}

type TaskSnapshot struct {
	Task          Task
	Attempt       Attempt
	SealRequest   *SealRequest
	Result        *Result
	Verifications []Verification
	Publication   *Publication
}

type DeliveryTransition struct {
	Task         Task
	Attempt      Attempt
	AttemptEvent Event
	TaskEvent    Event
}

type DeliveryPhaseTransition struct {
	Task    Task
	Attempt Attempt
	Event   Event
}

type ClaimPreparedAttemptParams struct {
	AttemptID      task.AttemptID
	LeaseOwner     string
	ClaimEventID   task.EventID
	TaskEventID    task.EventID
	Now            time.Time
	LeaseExpiresAt time.Time
	Actor          task.ActorSnapshot
}

type AdvanceDeliveryPhaseParams struct {
	AttemptID               task.AttemptID
	LeaseOwner              string
	ExpectedAttemptRevision int64
	From                    DeliveryPhase
	To                      DeliveryPhase
	EventID                 task.EventID
	Now                     time.Time
	Actor                   task.ActorSnapshot
}

type RecoverExpiredDeliveryClaimParams struct {
	AttemptID               task.AttemptID
	ExpiredLeaseOwner       string
	ExpectedAttemptRevision int64
	RecoveryEventID         task.EventID
	TaskEventID             task.EventID
	Now                     time.Time
	Reason                  string
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type RecordAdmissionParams struct {
	AttemptID               task.AttemptID
	LeaseOwner              string
	ExpectedAttemptRevision int64
	AttemptEventID          task.EventID
	TaskEventID             task.EventID
	Now                     time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type RecordDeliveryUncertainParams struct {
	AttemptID               task.AttemptID
	LeaseOwner              string
	ExpectedAttemptRevision int64
	AttemptEventID          task.EventID
	TaskEventID             task.EventID
	Now                     time.Time
	Reason                  string
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type RecordDeliveryRecoveryRequiredParams = RecordDeliveryUncertainParams

type ResolveUncertainDeliveryOutcome string

const (
	ResolveUncertainDeliveryAdmitted         ResolveUncertainDeliveryOutcome = "admitted"
	ResolveUncertainDeliveryRecoveryRequired ResolveUncertainDeliveryOutcome = "recovery_required"
)

func (o ResolveUncertainDeliveryOutcome) valid() bool {
	return o == ResolveUncertainDeliveryAdmitted || o == ResolveUncertainDeliveryRecoveryRequired
}

type ResolveUncertainDeliveryParams struct {
	AttemptID               task.AttemptID
	ExpectedAttemptRevision int64
	ExpectedTaskRevision    int64
	AttemptEventID          task.EventID
	TaskEventID             task.EventID
	Now                     time.Time
	Outcome                 ResolveUncertainDeliveryOutcome
	Reason                  string
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type ResumeUncertainPrePromptDeliveryParams struct {
	AttemptID               task.AttemptID
	ExpectedAttemptRevision int64
	ExpectedTaskRevision    int64
	ExpectedPhase           DeliveryPhase
	LeaseOwner              string
	LeaseExpiresAt          time.Time
	AttemptEventID          task.EventID
	TaskEventID             task.EventID
	Now                     time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

const PreparedAttemptDeadlineElapsed = "deadline_elapsed"

type ExpirePreparedAttemptParams struct {
	AttemptID               task.AttemptID
	ExpectedAttemptRevision int64
	ExpectedTaskRevision    int64
	AttemptEventID          task.EventID
	TaskEventID             task.EventID
	Now                     time.Time
	Actor                   task.ActorSnapshot
}

// JournalEvent is the immutable proof paired with one verification or
// publication revision. Journal events are separate from the reconnect event
// stream, whose migration-1 schema is deliberately closed to task/attempt IDs.
type JournalEvent struct {
	ID             task.EventID
	WorkspaceID    task.WorkspaceID
	TaskID         task.TaskID
	AttemptID      task.AttemptID
	ResultID       task.ResultID
	EntityType     string
	EntityID       string
	Type           string
	FromState      string
	ToState        string
	EntityRevision int64
	OccurredAt     time.Time
	Actor          task.ActorSnapshot
	EvidenceSHA256 [32]byte
	Payload        json.RawMessage
}

type VerificationState string

const (
	VerificationPrepared         VerificationState = "prepared"
	VerificationRunning          VerificationState = "running"
	VerificationSucceeded        VerificationState = "succeeded"
	VerificationFailed           VerificationState = "failed"
	VerificationRecoveryRequired VerificationState = "recovery_required"
)

type VerificationOutput struct {
	ByteCount     int64
	RetainedBytes int64
	SHA256        [32]byte
	Truncated     bool
}

type Verification struct {
	ID                task.VerificationID
	ResultID          task.ResultID
	TaskID            task.TaskID
	AttemptID         task.AttemptID
	WorkspaceID       task.WorkspaceID
	State             VerificationState
	PolicyName        string
	PolicySHA256      [32]byte
	VerifiedCommit    task.GitOID
	WorkingDirectory  string
	Timeout           time.Duration
	OutputLimitBytes  int64
	RunnerName        string
	RunnerVersion     string
	ImageDigest       string
	EnvironmentSHA256 [32]byte
	EffectAttempt     int
	StartedAt         *time.Time
	EndedAt           *time.Time
	Outcome           string
	ExitCode          *int
	Signal            string
	Stdout            *VerificationOutput
	Stderr            *VerificationOutput
	Reason            string
	LatestEventID     task.EventID
	Revision          int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type VerificationRecord struct {
	Verification Verification
	Event        JournalEvent
	Replayed     bool
}

type PrepareVerificationParams struct {
	VerificationID          task.VerificationID
	ResultID                task.ResultID
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	EventID                 task.EventID
	PolicyName              string
	PolicySHA256            [32]byte
	VerifiedCommit          task.GitOID
	WorkingDirectory        string
	Timeout                 time.Duration
	OutputLimitBytes        int64
	RunnerName              string
	RunnerVersion           string
	ImageDigest             string
	EnvironmentSHA256       [32]byte
	PreparedAt              time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type AdvanceVerificationParams struct {
	VerificationID          task.VerificationID
	ExpectedRevision        int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	EventID                 task.EventID
	StartedAt               time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type CompleteVerificationParams struct {
	VerificationID          task.VerificationID
	ExpectedRevision        int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	EventID                 task.EventID
	State                   VerificationState
	Outcome                 string
	ExitCode                *int
	Signal                  string
	Stdout                  VerificationOutput
	Stderr                  VerificationOutput
	Reason                  string
	EndedAt                 time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type RecoverVerificationParams struct {
	VerificationID          task.VerificationID
	ExpectedRevision        int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	EventID                 task.EventID
	Reason                  string
	Outcome                 string
	Stdout                  *VerificationOutput
	Stderr                  *VerificationOutput
	RecoveredAt             time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type PublicationState string
type PublicationPhase string

const (
	PublicationPrepared         PublicationState = "prepared"
	PublicationRunning          PublicationState = "running"
	PublicationUncertain        PublicationState = "uncertain"
	PublicationRecoveryRequired PublicationState = "recovery_required"
	PublicationPublished        PublicationState = "published"
	PublicationFailed           PublicationState = "failed"
	PublicationConflict         PublicationState = "conflict"

	PublicationPhaseNone            PublicationPhase = "none"
	PublicationPhasePushStarted     PublicationPhase = "push_started"
	PublicationPhasePushObserved    PublicationPhase = "push_observed"
	PublicationPhasePRCreateStarted PublicationPhase = "pr_create_started"
)

type Publication struct {
	ID                  task.PublicationID
	OperationID         task.PublicationOperationID
	AdmissionReceiptID  task.ReceiptID
	ResultID            task.ResultID
	VerificationID      task.VerificationID
	TaskID              task.TaskID
	AttemptID           task.AttemptID
	WorkspaceID         task.WorkspaceID
	State               PublicationState
	EffectPhase         PublicationPhase
	Tuple               task.PublicationTuple
	BrokerPolicyVersion string
	BrokerPolicySHA256  [32]byte
	ObservedRemoteSHA   task.GitOID
	Observation         *task.PublicationObservation
	Reason              string
	LatestEventID       task.EventID
	Requester           task.ActorSnapshot
	Revision            int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type PublicationRecord struct {
	Publication Publication
	Event       JournalEvent
	Replayed    bool
}

type PublicationAdmission struct {
	Publication Publication
	Receipt     Receipt
	Event       JournalEvent
	Replayed    bool
}

// PublicationWork is one consistent source snapshot for durable publication.
// Coordinators use these exact revisions and tuples rather than deriving them
// from a checkout or from independent mutable reads.
type PublicationWork struct {
	Publication  Publication
	Task         Task
	Attempt      Attempt
	Result       Result
	Verification Verification
	Event        JournalEvent
}

type PreparePublicationParams struct {
	PublicationID           task.PublicationID
	ResultID                task.ResultID
	VerificationID          task.VerificationID
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	EventID                 task.EventID
	Tuple                   task.PublicationTuple
	BrokerPolicyVersion     string
	BrokerPolicySHA256      [32]byte
	PreparedAt              time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type AdmitPublicationParams struct {
	PublicationID       task.PublicationID
	OperationID         task.PublicationOperationID
	ReceiptID           task.ReceiptID
	EventID             task.EventID
	ResultID            task.ResultID
	VerificationID      task.VerificationID
	Claim               task.IdempotencyClaim
	BrokerPolicyVersion string
	BrokerPolicySHA256  [32]byte
	APIContractVersion  string
	AcceptedAt          time.Time
}

type AdvancePublicationParams struct {
	PublicationID           task.PublicationID
	ExpectedRevision        int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	From                    PublicationPhase
	To                      PublicationPhase
	ObservedRemoteSHA       task.GitOID
	EventID                 task.EventID
	AdvancedAt              time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type CompletePublicationParams struct {
	PublicationID           task.PublicationID
	ExpectedRevision        int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	EventID                 task.EventID
	Observation             task.PublicationObservation
	CompletedAt             time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}

type RecoverPublicationParams struct {
	PublicationID           task.PublicationID
	ExpectedRevision        int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	EventID                 task.EventID
	State                   PublicationState
	Reason                  string
	RecoveredAt             time.Time
	EvidencePayload         json.RawMessage
	EvidenceSHA256          [32]byte
	Actor                   task.ActorSnapshot
}
