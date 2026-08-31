package taskstore

import (
	"encoding/json"
	"time"

	"github.com/nebler/fern/internal/task"
)

const SealBackgroundRunCommand = "run.seal"

type BackgroundRunSealRequest struct {
	ID                      task.SealRequestID
	ReceiptID               task.ReceiptID
	WorkspaceID             task.WorkspaceID
	TaskID                  task.TaskID
	AttemptID               task.AttemptID
	Generation              int64
	ExpectedRunRevision     int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	IdempotencyKey          task.IdempotencyKey
	RequestHash             task.RequestHash
	Owner                   task.ActorSnapshot
	ExportID                task.ArtifactExportID
	ArtifactID              task.RetainedArtifactID
	MaterializationID       task.MaterializationID
	ResultID                task.ResultID
	ResultEventID           task.EventID
	TaskEventID             task.EventID
	CommitEpochSeconds      int64
	PolicyVersion           string
	AcceptedAt              time.Time
}

type SealBackgroundRunParams struct {
	WorkspaceID             task.WorkspaceID
	TaskID                  task.TaskID
	AttemptID               task.AttemptID
	Generation              int64
	ExpectedRunRevision     int64
	ExpectedTaskRevision    int64
	ExpectedAttemptRevision int64
	SealRequestID           task.SealRequestID
	ReceiptID               task.ReceiptID
	ExportID                task.ArtifactExportID
	ArtifactID              task.RetainedArtifactID
	MaterializationID       task.MaterializationID
	ResultID                task.ResultID
	ResultEventID           task.EventID
	TaskEventID             task.EventID
	Claim                   task.IdempotencyClaim
	CommitEpochSeconds      int64
	PolicyVersion           string
	APIContractVersion      string
	AcceptedAt              time.Time
}

type BackgroundRunSealAdmission struct {
	Run      BackgroundRun
	Request  BackgroundRunSealRequest
	Export   BackgroundRunExport
	Receipt  Receipt
	Replayed bool
}

type BackgroundRunExportState string
type BackgroundRunExportPhase string

const (
	BackgroundRunExportPrepared         BackgroundRunExportState = "prepared"
	BackgroundRunExportRunning          BackgroundRunExportState = "running"
	BackgroundRunExportRecoveryRequired BackgroundRunExportState = "recovery_required"
	BackgroundRunExportCompleted        BackgroundRunExportState = "completed"

	BackgroundRunExportPhasePrepared           BackgroundRunExportPhase = "prepared"
	BackgroundRunExportPhaseSnapshotStarted    BackgroundRunExportPhase = "snapshot_started"
	BackgroundRunExportPhaseSnapshotSelected   BackgroundRunExportPhase = "snapshot_selected"
	BackgroundRunExportPhaseBundleWriteStarted BackgroundRunExportPhase = "bundle_write_started"
	BackgroundRunExportPhaseBundleVerified     BackgroundRunExportPhase = "bundle_verified"
	BackgroundRunExportPhaseCASInstallStarted  BackgroundRunExportPhase = "cas_install_started"
	BackgroundRunExportPhaseCASInstalled       BackgroundRunExportPhase = "cas_installed"
	BackgroundRunExportPhaseMaterializeStarted BackgroundRunExportPhase = "materialize_started"
	BackgroundRunExportPhaseMaterialized       BackgroundRunExportPhase = "materialized"
	BackgroundRunExportPhaseCompleted          BackgroundRunExportPhase = "completed"
)

type BackgroundRunExport struct {
	ID                     task.ArtifactExportID
	SealRequestID          task.SealRequestID
	WorkspaceID            task.WorkspaceID
	TaskID                 task.TaskID
	AttemptID              task.AttemptID
	Generation             int64
	ArtifactID             task.RetainedArtifactID
	MaterializationID      task.MaterializationID
	ResultID               task.ResultID
	State                  BackgroundRunExportState
	Phase                  BackgroundRunExportPhase
	ClaimOwner             string
	ClaimExpiresAt         *time.Time
	ClaimGeneration        int64
	RepositoryID           task.RepositoryID
	BaseSHA                task.GitOID
	OpenCodeSessionID      task.OpenCodeSessionID
	OpenCodeMessageID      task.OpenCodeMessageID
	ResultCommit           task.GitOID
	TreeOID                task.GitOID
	Outcome                task.ResultOutcome
	ResultManifest         []ManifestEntry
	ChangesSHA256          [32]byte
	ArtifactManifest       json.RawMessage
	ArtifactManifestSHA256 [32]byte
	CASLocator             string
	BundleSHA256           [32]byte
	BundleBytes            int64
	CollectedAt            *time.Time
	RecoveryReason         string
	Revision               int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type ClaimBackgroundRunExportParams struct {
	ExportID         task.ArtifactExportID
	TaskID           task.TaskID
	AttemptID        task.AttemptID
	Generation       int64
	ExpectedRevision int64
	ExpectedPhase    BackgroundRunExportPhase
	ClaimOwner       string
	Now              time.Time
	LeaseDuration    time.Duration
}

type BackgroundRunExportClaim struct {
	ExportID         task.ArtifactExportID
	TaskID           task.TaskID
	AttemptID        task.AttemptID
	Generation       int64
	ExpectedRevision int64
	ExpectedPhase    BackgroundRunExportPhase
	ClaimOwner       string
	ClaimGeneration  int64
	Now              time.Time
}

type SelectBackgroundRunSnapshotParams struct {
	BackgroundRunExportClaim
	ResultCommit           task.GitOID
	TreeOID                task.GitOID
	Outcome                task.ResultOutcome
	ResultManifest         []ManifestEntry
	ChangesSHA256          [32]byte
	ArtifactManifest       json.RawMessage
	ArtifactManifestSHA256 [32]byte
	OpenCodeSessionID      task.OpenCodeSessionID
	OpenCodeMessageID      task.OpenCodeMessageID
	CollectedAt            time.Time
}

type VerifyBackgroundRunBundleParams struct {
	BackgroundRunExportClaim
	BundleSHA256 [32]byte
	BundleBytes  int64
}

type WriterFenceKind string

const (
	WriterFenceNeverCreated   WriterFenceKind = "never_created"
	WriterFenceNeverStarted   WriterFenceKind = "never_started"
	WriterFenceRuntimeStopped WriterFenceKind = "runtime_stopped"
)

type WriterFence struct {
	SealRequestID      task.SealRequestID
	ExportID           task.ArtifactExportID
	TaskID             task.TaskID
	AttemptID          task.AttemptID
	Generation         int64
	Kind               WriterFenceKind
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	RuntimeToken       string
	StoppedAt          *time.Time
	ProofSHA256        [32]byte
	RecordedAt         time.Time
}

type RecordBackgroundRunWriterFenceParams struct {
	BackgroundRunClaim
	SealRequestID      task.SealRequestID
	ExportID           task.ArtifactExportID
	Kind               WriterFenceKind
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	RuntimeToken       string
	StoppedAt          *time.Time
	ProofSHA256        [32]byte
}

type ArtifactMaterializationState string

const (
	ArtifactMaterializationPrepared         ArtifactMaterializationState = "prepared"
	ArtifactMaterializationReady            ArtifactMaterializationState = "ready"
	ArtifactMaterializationRecoveryRequired ArtifactMaterializationState = "recovery_required"
)

type ArtifactMaterialization struct {
	ID             task.MaterializationID
	SealRequestID  task.SealRequestID
	ExportID       task.ArtifactExportID
	ArtifactID     task.RetainedArtifactID
	ResultID       task.ResultID
	State          ArtifactMaterializationState
	ResultCommit   task.GitOID
	TreeOID        task.GitOID
	ProofSHA256    [32]byte
	RecoveryReason string
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type RecordArtifactMaterializationReadyParams struct {
	BackgroundRunExportClaim
	MaterializationID task.MaterializationID
	ArtifactID        task.RetainedArtifactID
	ResultID          task.ResultID
	ResultCommit      task.GitOID
	TreeOID           task.GitOID
	ProofSHA256       [32]byte
}

type RetainedArtifact struct {
	ID                task.RetainedArtifactID
	SealRequestID     task.SealRequestID
	ExportID          task.ArtifactExportID
	MaterializationID task.MaterializationID
	ResultID          task.ResultID
	WorkspaceID       task.WorkspaceID
	TaskID            task.TaskID
	AttemptID         task.AttemptID
	Generation        int64
	Manifest          json.RawMessage
	ManifestSHA256    [32]byte
	ChangesSHA256     [32]byte
	CASLocator        string
	BundleSHA256      [32]byte
	BundleBytes       int64
	BaseSHA           task.GitOID
	ResultCommit      task.GitOID
	TreeOID           task.GitOID
	OpenCodeSessionID task.OpenCodeSessionID
	OpenCodeMessageID task.OpenCodeMessageID
	CommittedAt       time.Time
}

type CommitBackgroundRunRetainedResultParams struct {
	BackgroundRunExportClaim
	MaterializationID task.MaterializationID
	ArtifactID        task.RetainedArtifactID
	ResultID          task.ResultID
	ResultEventID     task.EventID
	TaskEventID       task.EventID
	EvidencePayload   json.RawMessage
	EvidenceSHA256    [32]byte
	Actor             task.ActorSnapshot
	SealedAt          time.Time
}

type BackgroundRunRetainedResult struct {
	SealedResult
	Run             BackgroundRun
	SealRequest     BackgroundRunSealRequest
	Export          BackgroundRunExport
	Artifact        RetainedArtifact
	Materialization ArtifactMaterialization
}
