package taskstore

import (
	"time"

	"github.com/nebler/fern/internal/task"
)

const (
	RequestBackgroundRunTakeoverCommand = "run.takeover"
	RequestBackgroundRunHandbackCommand = "run.handback"
	InterruptBackgroundRunCommand       = "run.interrupt"
	SteerBackgroundRunCommand           = "run.steer"
)

type BackgroundRunOwnershipMode string
type BackgroundRunOwnershipPhase string

const (
	BackgroundRunAgentOwned         BackgroundRunOwnershipMode = "agent_owned"
	BackgroundRunTakeoverRequested  BackgroundRunOwnershipMode = "takeover_requested"
	BackgroundRunHumanOwned         BackgroundRunOwnershipMode = "human_owned"
	BackgroundRunHandbackRequested  BackgroundRunOwnershipMode = "handback_requested"
	BackgroundRunOwnershipUncertain BackgroundRunOwnershipMode = "uncertain"
	BackgroundRunOwnershipClosed    BackgroundRunOwnershipMode = "closed"
)

const (
	BackgroundRunOwnershipAgentActive       BackgroundRunOwnershipPhase = "agent_active"
	BackgroundRunOwnershipAgentRouteRemoval BackgroundRunOwnershipPhase = "agent_route_removal"
	BackgroundRunOwnershipAgentStop         BackgroundRunOwnershipPhase = "agent_stop"
	BackgroundRunOwnershipAgentRemove       BackgroundRunOwnershipPhase = "agent_remove"
	BackgroundRunOwnershipAgentVolumeRemove BackgroundRunOwnershipPhase = "agent_volume_remove"
	BackgroundRunOwnershipHumanCreate       BackgroundRunOwnershipPhase = "human_create"
	BackgroundRunOwnershipHumanStart        BackgroundRunOwnershipPhase = "human_start"
	BackgroundRunOwnershipHumanActive       BackgroundRunOwnershipPhase = "human_active"
	BackgroundRunOwnershipHumanRouteRemoval BackgroundRunOwnershipPhase = "human_route_removal"
	BackgroundRunOwnershipHumanStop         BackgroundRunOwnershipPhase = "human_stop"
	BackgroundRunOwnershipHumanRemove       BackgroundRunOwnershipPhase = "human_remove"
	BackgroundRunOwnershipAgentVolumeCreate BackgroundRunOwnershipPhase = "agent_volume_create"
	BackgroundRunOwnershipAgentCreate       BackgroundRunOwnershipPhase = "agent_create"
	BackgroundRunOwnershipAgentStart        BackgroundRunOwnershipPhase = "agent_start"
	BackgroundRunOwnershipAgentHealth       BackgroundRunOwnershipPhase = "agent_health"
	BackgroundRunOwnershipAgentSession      BackgroundRunOwnershipPhase = "agent_session"
	BackgroundRunOwnershipAgentPrompt       BackgroundRunOwnershipPhase = "agent_prompt"
	BackgroundRunOwnershipUncertainPhase    BackgroundRunOwnershipPhase = "uncertain"
	BackgroundRunOwnershipClosedPhase       BackgroundRunOwnershipPhase = "closed"
)

type BackgroundRunOwnership struct {
	WorkspaceID      task.WorkspaceID
	TaskID           task.TaskID
	AttemptID        task.AttemptID
	RunGeneration    int64
	Mode             BackgroundRunOwnershipMode
	Phase            BackgroundRunOwnershipPhase
	WriterGeneration int64

	ContainerIdentity  string
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	RuntimeToken       string
	VolumeIdentity     string
	EndpointIdentity   string
	HostPort           int
	OpenCodeSessionID  task.OpenCodeSessionID
	OpenCodeMessageID  task.OpenCodeMessageID

	TargetWriterGeneration  int64
	TargetContainerIdentity string
	TargetVolumeIdentity    string
	TargetEndpointIdentity  string
	TargetOpenCodeSessionID task.OpenCodeSessionID
	TargetOpenCodeMessageID task.OpenCodeMessageID

	RequestReceiptID task.ReceiptID
	RequestActor     *task.ActorSnapshot
	RequestedAt      *time.Time
	RouteEvidence    string
	WriterEvidence   string
	ResourceEvidence string
	GitEvidence      string
	LastError        string

	ClaimOwner      string
	ClaimExpiresAt  *time.Time
	ClaimGeneration int64
	Revision        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type RequestBackgroundRunTakeoverParams struct {
	WorkspaceID        task.WorkspaceID
	TaskID             task.TaskID
	ReceiptID          task.ReceiptID
	Claim              task.IdempotencyClaim
	APIContractVersion string
	RequestedAt        time.Time
}

type RequestBackgroundRunHandbackParams struct {
	WorkspaceID        task.WorkspaceID
	TaskID             task.TaskID
	ReceiptID          task.ReceiptID
	Claim              task.IdempotencyClaim
	APIContractVersion string
	RequestedAt        time.Time
}

type BackgroundRunOwnershipAdmission struct {
	Ownership BackgroundRunOwnership
	Receipt   Receipt
	Replayed  bool
}

type BackgroundRunOwnershipWork struct {
	Run       BackgroundRun
	Prompt    string
	Ownership BackgroundRunOwnership
}

type BackgroundRunControlView struct {
	Run       BackgroundRun
	Ownership BackgroundRunOwnership
}

type BackgroundRunControlState string

const (
	BackgroundRunControlRequested BackgroundRunControlState = "requested"
	BackgroundRunControlAttempted BackgroundRunControlState = "attempted"
	BackgroundRunControlSucceeded BackgroundRunControlState = "succeeded"
	BackgroundRunControlUncertain BackgroundRunControlState = "uncertain"
	BackgroundRunControlConflict  BackgroundRunControlState = "conflict"
)

type BackgroundRunControl struct {
	ReceiptID          task.ReceiptID
	WorkspaceID        task.WorkspaceID
	TaskID             task.TaskID
	AttemptID          task.AttemptID
	RunGeneration      int64
	CommandKind        string
	State              BackgroundRunControlState
	WriterGeneration   int64
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	RuntimeToken       string
	OpenCodeSessionID  task.OpenCodeSessionID
	OpenCodeMessageID  task.OpenCodeMessageID
	Instruction        string
	AttemptedAt        *time.Time
	CompletedAt        *time.Time
	LastError          string
	ClaimOwner         string
	ClaimExpiresAt     *time.Time
	ClaimGeneration    int64
	Revision           int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type AdmitBackgroundRunControlParams struct {
	WorkspaceID        task.WorkspaceID
	TaskID             task.TaskID
	ReceiptID          task.ReceiptID
	OpenCodeMessageID  task.OpenCodeMessageID
	Instruction        string
	Claim              task.IdempotencyClaim
	APIContractVersion string
	RequestedAt        time.Time
}

type BackgroundRunControlAdmission struct {
	Control  BackgroundRunControl
	Receipt  Receipt
	Replayed bool
}

type ClaimNextBackgroundRunControlParams struct {
	WorkspaceID   task.WorkspaceID
	ClaimOwner    string
	Now           time.Time
	LeaseDuration time.Duration
}

type BackgroundRunControlClaim struct {
	WorkspaceID      task.WorkspaceID
	ReceiptID        task.ReceiptID
	ExpectedRevision int64
	ExpectedState    BackgroundRunControlState
	ClaimOwner       string
	ClaimGeneration  int64
	Now              time.Time
}

type BackgroundRunControlWork struct {
	Run       BackgroundRun
	Ownership BackgroundRunOwnership
	Control   BackgroundRunControl
}

type ClaimNextBackgroundRunOwnershipParams struct {
	WorkspaceID   task.WorkspaceID
	ClaimOwner    string
	Now           time.Time
	LeaseDuration time.Duration
}

type BackgroundRunOwnershipClaim struct {
	WorkspaceID      task.WorkspaceID
	TaskID           task.TaskID
	AttemptID        task.AttemptID
	RunGeneration    int64
	ExpectedRevision int64
	ExpectedMode     BackgroundRunOwnershipMode
	ExpectedPhase    BackgroundRunOwnershipPhase
	ClaimOwner       string
	ClaimGeneration  int64
	Now              time.Time
}

type AdvanceBackgroundRunOwnershipParams struct {
	BackgroundRunOwnershipClaim
	Mode               BackgroundRunOwnershipMode
	Phase              BackgroundRunOwnershipPhase
	WriterGeneration   int64
	ContainerIdentity  string
	ContainerID        string
	ContainerStartedAt string
	RuntimeEpoch       int64
	RuntimeToken       string
	VolumeIdentity     string
	EndpointIdentity   string
	HostPort           int
	OpenCodeSessionID  task.OpenCodeSessionID
	OpenCodeMessageID  task.OpenCodeMessageID
	RouteEvidence      string
	WriterEvidence     string
	ResourceEvidence   string
	GitEvidence        string
	LastError          string
}
