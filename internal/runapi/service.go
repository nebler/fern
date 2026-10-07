package runapi

// This file owns the committed background-run commands (create, stop, seal):
// intent validation, private request-hash encoding, idempotent replay, durable
// admission, and post-commit wake. Authentication, HTTP parsing, and response
// encoding stay in runapi.go.

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

// maxPromptBytes and maxPromptRunes bound a background run instruction. The
// HTTP boundary is the only check; the schema's 64 KiB CHECK is a storage
// backstop.
const (
	maxPromptBytes = 16 << 10
	maxPromptRunes = 4000
)

var (
	errProfileUnavailable = errors.New("background profile unavailable")
	errBaseUnavailable    = errors.New("background base unavailable")
	errReplayConflict     = errors.New("idempotency key already used for another request")
	errInvalidCreate      = errors.New("invalid run creation input")
	errInvalidBase        = errors.New("invalid exact base commit")
)

// BaseVerifier proves an exact commit is reachable from an allowed repository ref.
type BaseVerifier interface {
	Verify(context.Context, task.GitOID) error
}

// service runs the commands against the handler's validated Config. New
// performs all configuration validation before a service is constructed.
type service struct{ config Config }

// createIntent is the caller's intent, not the HTTP or persisted hash schema.
type createIntent struct {
	Repository  string
	BaseOID     string
	Branch      *string
	Instruction string
	Profile     string
}

type createAcceptance struct {
	RunID               task.TaskID
	Committed, Replayed bool
}
type stopAcceptance struct {
	RunID    task.TaskID
	State    run.State
	Replayed bool
}
type sealAcceptance struct {
	RunID               task.TaskID
	State               run.State
	ResultPhase         string
	ResultID            task.ResultID
	Committed, Replayed bool
}

func commandHash(kind string, value any) task.RequestHash {
	encoded, _ := json.Marshal(value)
	return task.RequestHash(sha256.Sum256(append(append([]byte(kind), '\n'), encoded...)))
}

func (s *service) claim(actor task.ActorSnapshot, key task.IdempotencyKey, kind string, value any) (task.IdempotencyClaim, error) {
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

func (s *service) replay(ctx context.Context, claim task.IdempotencyClaim) (taskstore.Receipt, bool, error) {
	receipt, found, err := s.config.Store.FindReceiptByIdempotency(ctx, s.config.WorkspaceID, claim.Scope.CommandKind, claim.Key)
	if err != nil || !found {
		return receipt, found, err
	}
	existing := receipt.Claim()
	disposition := task.ClassifyIdempotency(&existing, claim)
	if disposition == task.IdempotencyOwnerMismatch {
		return receipt, true, taskstore.ErrNotFound
	}
	if disposition != task.IdempotencyReplay {
		return receipt, true, errReplayConflict
	}
	return receipt, true, nil
}

func (s *service) notify(replayed bool) {
	if !replayed && s.config.Wake != nil {
		s.config.Wake()
	}
}

func (s *service) Create(ctx context.Context, actor task.ActorSnapshot, key task.IdempotencyKey, input createIntent) (createAcceptance, error) {
	var zero createAcceptance
	if input.Repository != s.config.RepositoryRemote || input.Profile != run.SourceProfile || !validInstruction(input.Instruction) || !validBranchDisplay(input.Branch) {
		return zero, errInvalidCreate
	}
	base, err := task.ParseGitOID(input.BaseOID)
	if err != nil {
		return zero, errInvalidBase
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
		current, err := s.config.Store.GetBackgroundRun(ctx, s.config.WorkspaceID, receipt.RunID, actor)
		if err != nil {
			return zero, err
		}
		return createAcceptance{current.TaskID, true, true}, nil
	}
	if s.config.AvailableProfile != taskstore.BackgroundRunSourceProfile {
		return zero, errProfileUnavailable
	}
	if err := s.config.BaseVerifier.Verify(ctx, base); err != nil {
		return zero, errBaseUnavailable
	}
	ids, err := s.config.Generator.GenerateAdmissionIDs()
	if err != nil {
		return zero, err
	}
	now := s.config.Now().UTC().Truncate(time.Millisecond)
	if now.IsZero() || now.UnixMilli() < 0 {
		return zero, errors.New("invalid command clock")
	}
	branch := ""
	if input.Branch != nil {
		branch = *input.Branch
	}
	admission, err := s.config.Store.AdmitBackgroundRun(ctx, taskstore.AdmitBackgroundRunParams{
		TaskID: ids.TaskID, OpenCodeSessionID: ids.OpenCodeSessionID, OpenCodeMessageID: ids.OpenCodeMessageID,
		Claim: claim, Prompt: input.Instruction, RepositoryID: s.config.RepositoryID,
		RepositoryRemote: input.Repository, BaseSHA: base, Branch: branch, Profile: input.Profile,
		ImageIdentity: s.config.BackgroundImageIdentity, EnvironmentSHA256: s.config.BackgroundEnvironmentSHA256,
		Agent: s.config.Agent, ModelProvider: s.config.ModelProvider, Model: s.config.Model,
		Deadline: now.Add(s.config.AttemptTimeout), APIContractVersion: APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		return zero, err
	}
	s.notify(admission.Replayed)
	return createAcceptance{admission.Run.TaskID, true, admission.Replayed}, nil
}

func validInstruction(value string) bool {
	if len(value) < 1 || len(value) > maxPromptBytes || utf8.RuneCountInString(value) > maxPromptRunes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
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

func (s *service) mutationClaim(actor task.ActorSnapshot, key task.IdempotencyKey, kind string, id task.TaskID) (task.IdempotencyClaim, error) {
	if _, err := task.ParseTaskID(string(id)); err != nil {
		return task.IdempotencyClaim{}, err
	}
	return s.claim(actor, key, kind, struct {
		RunID task.TaskID `json:"run_id"`
	}{id})
}

// stopReplay projects the original committed acceptance, not today's run state.
func stopReplay(id task.TaskID, current taskstore.BackgroundRun, receipt taskstore.Receipt) (stopAcceptance, error) {
	var committed struct {
		RunID task.TaskID `json:"run_id"`
		State run.State   `json:"state"`
	}
	if receipt.RunID != id || current.StopReceiptID != receipt.ID || json.Unmarshal(receipt.ResponseProjection, &committed) != nil || committed.RunID != id ||
		(committed.State != run.Failed && committed.State != run.Canceling) {
		return stopAcceptance{}, taskstore.ErrCorruptStore
	}
	return stopAcceptance{id, committed.State, true}, nil
}

func (s *service) Stop(ctx context.Context, actor task.ActorSnapshot, key task.IdempotencyKey, id task.TaskID) (stopAcceptance, error) {
	var zero stopAcceptance
	claim, err := s.mutationClaim(actor, key, taskstore.StopBackgroundRunCommand, id)
	if err != nil {
		return zero, err
	}
	receipt, found, err := s.replay(ctx, claim)
	if err != nil {
		return zero, err
	}
	if found {
		if receipt.RunID != id {
			return zero, errReplayConflict
		}
		current, err := s.config.Store.GetBackgroundRun(ctx, s.config.WorkspaceID, id, actor)
		if err != nil {
			return zero, err
		}
		return stopReplay(id, current, receipt)
	}
	result, err := s.config.Store.StopBackgroundRun(ctx, taskstore.StopBackgroundRunParams{WorkspaceID: s.config.WorkspaceID,
		TaskID: id, Claim: claim,
		APIContractVersion: APIContractVersion, StoppedAt: s.config.Now().UTC().Truncate(time.Millisecond)})
	if err != nil {
		return zero, err
	}
	if result.Replayed {
		return stopReplay(id, result.Run, result.Receipt)
	}
	s.notify(false)
	return stopAcceptance{id, run.State(result.Run.State), false}, nil
}

func (s *service) Seal(ctx context.Context, actor task.ActorSnapshot, key task.IdempotencyKey, id task.TaskID) (sealAcceptance, error) {
	var zero sealAcceptance
	claim, err := s.mutationClaim(actor, key, taskstore.SealBackgroundRunCommand, id)
	if err != nil {
		return zero, err
	}
	current, err := s.config.Store.GetBackgroundRun(ctx, s.config.WorkspaceID, id, actor)
	if err != nil {
		return zero, err
	}
	resultID, err := s.config.Generator.ResultID()
	if err != nil {
		return zero, err
	}
	now := s.config.Now().UTC().Truncate(time.Millisecond)
	admission, err := s.config.Store.SealBackgroundRun(ctx, taskstore.SealBackgroundRunParams{
		WorkspaceID: s.config.WorkspaceID, TaskID: current.TaskID, ExpectedRunRevision: current.Revision,
		ResultID: resultID, Claim: claim, PolicyVersion: s.config.SealPolicyVersion,
		APIContractVersion: APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		return zero, err
	}
	state, phase := run.Canceling, "seal_requested"
	if admission.Run.State == taskstore.BackgroundRunResultReady {
		state, phase = run.ResultReady, "ready"
	}
	s.notify(admission.Replayed)
	return sealAcceptance{admission.Run.TaskID, state, phase, admission.Run.Seal.ResultID, true, admission.Replayed}, nil
}
