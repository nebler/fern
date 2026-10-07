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
