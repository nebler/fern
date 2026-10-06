// Package runcommand owns committed background-run application commands.
// Authentication, HTTP parsing, and response encoding belong to its caller.
package runcommand

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const APIContractVersion = "fern.background-run.v1"

var (
	ErrProfileUnavailable = errors.New("background profile unavailable")
	ErrBaseUnavailable    = errors.New("background base unavailable")
	ErrReplayConflict     = errors.New("idempotency key already used for another request")
	ErrInvalidCreate      = errors.New("invalid run creation input")
	ErrInvalidBase        = errors.New("invalid exact base commit")
)

type Store interface {
	AdmitBackgroundRun(context.Context, taskstore.AdmitBackgroundRunParams) (taskstore.Admission, error)
	FindReceiptByIdempotency(context.Context, task.WorkspaceID, string, task.IdempotencyKey) (taskstore.Receipt, bool, error)
	GetBackgroundRun(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRun, error)
	StopBackgroundRun(context.Context, taskstore.StopBackgroundRunParams) (taskstore.BackgroundRunStop, error)
	SealBackgroundRun(context.Context, taskstore.SealBackgroundRunParams) (taskstore.BackgroundRunSealAdmission, error)
	GetBackgroundRunOwners(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.Task, taskstore.Attempt, error)
}

// BaseVerifier proves an exact commit is reachable from an allowed repository ref.
type BaseVerifier interface {
	Verify(context.Context, task.GitOID) error
}

type Config struct {
	WorkspaceID                 task.WorkspaceID
	RepositoryID                task.RepositoryID
	RepositoryRemote            string
	BackgroundImageIdentity     string
	BackgroundEnvironmentSHA256 [32]byte
	AvailableProfile            string
	Store                       Store
	Generator                   *task.Generator
	BaseVerifier                BaseVerifier
	Now                         func() time.Time
	AttemptTimeout              time.Duration
	Agent, ModelProvider, Model string
	Wake                        func()
	SealPolicyVersion           string
}

type Service struct{ config Config }

// New captures command policy. Deployment qualification remains the composition
// root's responsibility; only dependencies needed to execute are checked here.
func New(config Config) (*Service, error) {
	if config.Store == nil || config.Generator == nil || config.BaseVerifier == nil || config.Now == nil {
		return nil, errors.New("run command dependencies are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, err
	}
	if config.RepositoryRemote == "" {
		return nil, errors.New("bound repository remote is required")
	}
	return &Service{config: config}, nil
}

// CreateInput is the caller's intent, not the HTTP or persisted hash schema.
type CreateInput struct {
	Repository  string
	BaseOID     string
	Branch      *string
	Instruction string
	Profile     string
}

type CreateAcceptance struct {
	RunID               task.TaskID
	Committed, Replayed bool
}
type StopAcceptance struct {
	RunID    task.TaskID
	State    run.State
	Replayed bool
}
type SealAcceptance struct {
	RunID               task.TaskID
	State               run.State
	ResultPhase         string
	SealRequestID       task.SealRequestID
	Committed, Replayed bool
}

func commandHash(kind string, value any) task.RequestHash {
	encoded, _ := json.Marshal(value)
	return task.RequestHash(sha256.Sum256(append(append([]byte(kind), '\n'), encoded...)))
}

func (s *Service) claim(actor task.ActorSnapshot, key task.IdempotencyKey, kind string, value any) (task.IdempotencyClaim, error) {
	if err := actor.Validate(); err != nil {
		return task.IdempotencyClaim{}, err
	}
	if actor.Type != task.ActorOpenCode {
		return task.IdempotencyClaim{}, task.ErrInvalidActor
	}
	if _, err := task.ParseIdempotencyKey(string(key)); err != nil {
		return task.IdempotencyClaim{}, err
	}
	return task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: s.config.WorkspaceID, CommandKind: kind}, Key: key, RequestHash: commandHash(kind, value), Actor: actor}, nil
}

func (s *Service) replay(ctx context.Context, claim task.IdempotencyClaim) (taskstore.Receipt, bool, error) {
	receipt, found, err := s.config.Store.FindReceiptByIdempotency(ctx, s.config.WorkspaceID, claim.Scope.CommandKind, claim.Key)
	if err != nil || !found {
		return receipt, found, err
	}
	disposition, err := task.ClassifyIdempotency(&task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: receipt.WorkspaceID, CommandKind: receipt.CommandKind}, Key: receipt.IdempotencyKey, RequestHash: receipt.RequestHash, Actor: receipt.Actor}, claim)
	if err != nil {
		return receipt, true, err
	}
	if disposition == task.IdempotencyOwnerMismatch {
		return receipt, true, taskstore.ErrNotFound
	}
	if disposition != task.IdempotencyReplay {
		return receipt, true, ErrReplayConflict
	}
	return receipt, true, nil
}

func (s *Service) notify(replayed bool) {
	if !replayed && s.config.Wake != nil {
		s.config.Wake()
	}
}

func (s *Service) Create(ctx context.Context, actor task.ActorSnapshot, key task.IdempotencyKey, input CreateInput) (CreateAcceptance, error) {
	var zero CreateAcceptance
	if input.Repository != s.config.RepositoryRemote || input.Profile != run.SourceProfile || !validInstruction(input.Instruction) || !validBranchDisplay(input.Branch) {
		return zero, ErrInvalidCreate
	}
	base, err := task.ParseGitOID(input.BaseOID)
	if err != nil {
		return zero, ErrInvalidBase
	}
	// Preserve the original v1 bytes (including order, escaping, and null
	// branch) privately. Domain field names are not an idempotency protocol.
	payload := struct {
		Repository  string  `json:"repository"`
		BaseOID     string  `json:"base_oid"`
		Branch      *string `json:"branch"`
		Instruction string  `json:"instruction"`
		Profile     string  `json:"profile"`
	}{input.Repository, input.BaseOID, input.Branch, input.Instruction, input.Profile}
	claim, err := s.claim(actor, key, taskstore.CreateBackgroundRunCommand, payload)
	if err != nil {
		return zero, err
	}
	receipt, found, err := s.replay(ctx, claim)
	if err != nil {
		return zero, err
	}
	if found {
		current, err := s.config.Store.GetBackgroundRun(ctx, s.config.WorkspaceID, receipt.TargetID, actor)
		if err != nil {
			return zero, err
		}
		return CreateAcceptance{current.TaskID, true, true}, nil
	}
	if s.config.AvailableProfile != taskstore.BackgroundRunSourceProfile {
		return zero, ErrProfileUnavailable
	}
	if err := s.config.BaseVerifier.Verify(ctx, base); err != nil {
		return zero, ErrBaseUnavailable
	}
	ids, err := s.config.Generator.GenerateAdmissionIDs()
	if err != nil {
		return zero, err
	}
	now := s.config.Now().UTC().Truncate(time.Millisecond)
	if now.IsZero() || now.UnixMilli() < 0 {
		return zero, errors.New("invalid command clock")
	}
	branch, baseRef := "", input.BaseOID
	if input.Branch != nil {
		branch, baseRef = *input.Branch, *input.Branch
	}
	resources, err := run.NewResources(ids.TaskID, 1)
	if err != nil {
		return zero, err
	}
	intent := &taskstore.BackgroundRunIntent{RepositoryRemote: input.Repository, Branch: branch,
		InstructionSHA256: sha256.Sum256([]byte(input.Instruction)), Profile: input.Profile, ProfileSHA256: sha256.Sum256([]byte(input.Profile)),
		EnvironmentSHA256: s.config.BackgroundEnvironmentSHA256, ImageIdentity: s.config.BackgroundImageIdentity,
		CloneIdentity: resources.Clone(), VolumeIdentity: resources.Volume(), ContainerIdentity: resources.Container(), EndpointIdentity: resources.Endpoint()}
	admission, err := s.config.Store.AdmitBackgroundRun(ctx, taskstore.AdmitBackgroundRunParams{
		TaskID: ids.TaskID, AttemptID: ids.AttemptID, ReceiptID: ids.ReceiptID, TaskEventID: ids.TaskEventID,
		AttemptEventID: ids.AttemptEventID, OpenCodeSessionID: ids.OpenCodeSessionID, OpenCodeMessageID: ids.OpenCodeMessageID,
		Claim: claim, Title: "Background Run", Prompt: input.Instruction, RepositoryID: s.config.RepositoryID,
		BaseRef: baseRef, BaseSHA: base, ObjectFormat: "sha1", ExecutionContractVersion: APIContractVersion,
		Agent: s.config.Agent, ModelProvider: s.config.ModelProvider, Model: s.config.Model,
		Deadline: now.Add(s.config.AttemptTimeout), APIContractVersion: APIContractVersion,
		AcceptedAt: now, BackgroundRun: intent,
	})
	if err != nil {
		return zero, err
	}
	s.notify(admission.Replayed)
	return CreateAcceptance{admission.Task.ID, true, admission.Replayed}, nil
}

func validInstruction(value string) bool {
	if len(value) < 1 || len(value) > taskstore.MaxPromptBytes || utf8.RuneCountInString(value) > taskstore.MaxPromptRunes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) && char != '\n' && char != '\t' {
			return false
		}
	}
	return true
}

func validBranchDisplay(branch *string) bool {
	if branch == nil {
		return true
	}
	if len(*branch) < 1 || len(*branch) > 255 || !utf8.ValidString(*branch) {
		return false
	}
	for _, char := range *branch {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func (s *Service) mutationClaim(actor task.ActorSnapshot, key task.IdempotencyKey, kind string, id task.TaskID) (task.IdempotencyClaim, error) {
	if _, err := task.ParseTaskID(string(id)); err != nil {
		return task.IdempotencyClaim{}, err
	}
	return s.claim(actor, key, kind, struct {
		RunID task.TaskID `json:"run_id"`
	}{id})
}

// stopReplay projects the original committed acceptance, not today's run state.
func stopReplay(id task.TaskID, current taskstore.BackgroundRun, receipt taskstore.Receipt) (StopAcceptance, error) {
	var committed struct {
		RunID task.TaskID `json:"run_id"`
		State run.State   `json:"state"`
	}
	if receipt.TargetID != id || current.StopReceiptID != receipt.ID || json.Unmarshal(receipt.ResponseProjection, &committed) != nil || committed.RunID != id ||
		(committed.State != run.Failed && committed.State != run.Canceling) {
		return StopAcceptance{}, taskstore.ErrCorruptStore
	}
	return StopAcceptance{id, committed.State, true}, nil
}

func (s *Service) Stop(ctx context.Context, actor task.ActorSnapshot, key task.IdempotencyKey, id task.TaskID) (StopAcceptance, error) {
	var zero StopAcceptance
	claim, err := s.mutationClaim(actor, key, taskstore.StopBackgroundRunCommand, id)
	if err != nil {
		return zero, err
	}
	receipt, found, err := s.replay(ctx, claim)
	if err != nil {
		return zero, err
	}
	if found {
		if receipt.TargetID != id {
			return zero, ErrReplayConflict
		}
		current, err := s.config.Store.GetBackgroundRun(ctx, s.config.WorkspaceID, id, actor)
		if err != nil {
			return zero, err
		}
		return stopReplay(id, current, receipt)
	}
	receiptID, err := s.config.Generator.ReceiptID()
	if err != nil {
		return zero, err
	}
	attemptEventID, err := s.config.Generator.EventID()
	if err != nil {
		return zero, err
	}
	taskEventID, err := s.config.Generator.EventID()
	if err != nil {
		return zero, err
	}
	result, err := s.config.Store.StopBackgroundRun(ctx, taskstore.StopBackgroundRunParams{WorkspaceID: s.config.WorkspaceID,
		TaskID: id, ReceiptID: receiptID, AttemptEventID: attemptEventID, TaskEventID: taskEventID, Claim: claim,
		APIContractVersion: APIContractVersion, StoppedAt: s.config.Now().UTC().Truncate(time.Millisecond)})
	if err != nil {
		return zero, err
	}
	if result.Replayed {
		return stopReplay(id, result.Run, result.Receipt)
	}
	s.notify(false)
	return StopAcceptance{id, run.State(result.Run.State), false}, nil
}

func (s *Service) Seal(ctx context.Context, actor task.ActorSnapshot, key task.IdempotencyKey, id task.TaskID) (SealAcceptance, error) {
	var zero SealAcceptance
	claim, err := s.mutationClaim(actor, key, taskstore.SealBackgroundRunCommand, id)
	if err != nil {
		return zero, err
	}
	current, err := s.config.Store.GetBackgroundRun(ctx, s.config.WorkspaceID, id, actor)
	if err != nil {
		return zero, err
	}
	owner, attempt, err := s.config.Store.GetBackgroundRunOwners(ctx, s.config.WorkspaceID, id, actor)
	if err != nil {
		return zero, err
	}
	ids, err := s.config.Generator.GenerateBackgroundSealIDs()
	if err != nil {
		return zero, err
	}
	now := s.config.Now().UTC().Truncate(time.Millisecond)
	admission, err := s.config.Store.SealBackgroundRun(ctx, taskstore.SealBackgroundRunParams{
		WorkspaceID: s.config.WorkspaceID, TaskID: current.TaskID, AttemptID: current.AttemptID, Generation: current.Generation,
		ExpectedRunRevision: current.Revision, ExpectedTaskRevision: owner.Revision, ExpectedAttemptRevision: attempt.Revision,
		SealRequestID: ids.SealRequestID, ReceiptID: ids.ReceiptID, ExportID: ids.ArtifactExportID,
		ArtifactID: ids.RetainedArtifactID, MaterializationID: ids.MaterializationID, ResultID: ids.ResultID,
		ResultEventID: ids.ResultEventID, TaskEventID: ids.TaskEventID, Claim: claim, CommitEpochSeconds: now.Unix(),
		PolicyVersion: s.config.SealPolicyVersion, APIContractVersion: APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		return zero, err
	}
	state, phase := run.Canceling, "seal_requested"
	if admission.Run.State == taskstore.BackgroundRunResultReady {
		state, phase = run.ResultReady, "ready"
	}
	s.notify(admission.Replayed)
	return SealAcceptance{admission.Run.TaskID, state, phase, admission.Request.ID, true, admission.Replayed}, nil
}
