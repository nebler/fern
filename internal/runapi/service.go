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

	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/store"
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
	errBaseUnavailable = errors.New("background base unavailable")
	errReplayConflict  = errors.New("idempotency key already used for another request")
	errInvalidCreate   = errors.New("invalid run creation input")
	errInvalidBase     = errors.New("invalid exact base commit")
)

// BaseVerifier proves an exact commit is reachable from an allowed repository ref.
type BaseVerifier interface {
	Verify(context.Context, domain.GitOID) error
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
	RunID               domain.RunID
	Committed, Replayed bool
}
type stopAcceptance struct {
	RunID    domain.RunID
	State    domain.State
	Replayed bool
}
type sealAcceptance struct {
	RunID               domain.RunID
	State               domain.State
	ResultPhase         string
	ResultID            domain.ResultID
	Committed, Replayed bool
}

func commandHash(kind string, value any) domain.RequestHash {
	encoded, _ := json.Marshal(value)
	return domain.RequestHash(sha256.Sum256(append(append([]byte(kind), '\n'), encoded...)))
}

func (s *service) claim(actor domain.ActorSnapshot, key domain.IdempotencyKey, kind string, value any) (domain.IdempotencyClaim, error) {
	if err := actor.Validate(); err != nil {
		return domain.IdempotencyClaim{}, err
	}
	if actor.Type != domain.ActorOpenCode {
		return domain.IdempotencyClaim{}, domain.ErrInvalidActor
	}
	if _, err := domain.ParseIdempotencyKey(string(key)); err != nil {
		return domain.IdempotencyClaim{}, err
	}
	return domain.IdempotencyClaim{Scope: domain.IdempotencyScope{WorkspaceID: s.config.WorkspaceID, CommandKind: kind}, Key: key, RequestHash: commandHash(kind, value), Actor: actor}, nil
}

func (s *service) replay(ctx context.Context, claim domain.IdempotencyClaim) (store.Receipt, bool, error) {
	receipt, found, err := s.config.Store.FindReceiptByIdempotency(ctx, s.config.WorkspaceID, claim.Scope.CommandKind, claim.Key)
	if err != nil || !found {
		return receipt, found, err
	}
	existing := receipt.Claim()
	disposition := domain.ClassifyIdempotency(&existing, claim)
	if disposition == domain.IdempotencyOwnerMismatch {
		return receipt, true, store.ErrNotFound
	}
	if disposition != domain.IdempotencyReplay {
		return receipt, true, errReplayConflict
	}
	return receipt, true, nil
}

func (s *service) notify(replayed bool) {
	if !replayed && s.config.Wake != nil {
		s.config.Wake()
	}
}

func (s *service) Create(ctx context.Context, actor domain.ActorSnapshot, key domain.IdempotencyKey, input createIntent) (createAcceptance, error) {
	var zero createAcceptance
	if input.Repository != s.config.RepositoryRemote || input.Profile != domain.SourceProfile || !validInstruction(input.Instruction) || !validBranchDisplay(input.Branch) {
		return zero, errInvalidCreate
	}
	base, err := domain.ParseGitOID(input.BaseOID)
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
	claim, err := s.claim(actor, key, store.CreateBackgroundRunCommand, payload)
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
		return createAcceptance{current.RunID, true, true}, nil
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
	admission, err := s.config.Store.AdmitBackgroundRun(ctx, store.AdmitBackgroundRunParams{
		RunID: ids.RunID, OpenCodeSessionID: ids.OpenCodeSessionID, OpenCodeMessageID: ids.OpenCodeMessageID,
		Claim: claim, Prompt: input.Instruction, RepositoryID: s.config.RepositoryID,
		RepositoryRemote: input.Repository, BaseSHA: base, Branch: branch, Profile: input.Profile,
		ImageIdentity: s.config.BackgroundImageIdentity, EnvironmentSHA256: s.config.BackgroundEnvironmentSHA256,
		Agent: s.config.Agent, ModelProvider: s.config.ModelProvider, Model: s.config.Model,
		Deadline: now.Add(s.config.RunTimeout), APIContractVersion: APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		return zero, err
	}
	s.notify(admission.Replayed)
	return createAcceptance{admission.Run.RunID, true, admission.Replayed}, nil
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

func (s *service) mutationClaim(actor domain.ActorSnapshot, key domain.IdempotencyKey, kind string, id domain.RunID) (domain.IdempotencyClaim, error) {
	if _, err := domain.ParseRunID(string(id)); err != nil {
		return domain.IdempotencyClaim{}, err
	}
	return s.claim(actor, key, kind, struct {
		RunID domain.RunID `json:"run_id"`
	}{id})
}

// stopReplay projects the original committed acceptance, not today's run state.
func stopReplay(id domain.RunID, current store.BackgroundRun, receipt store.Receipt) (stopAcceptance, error) {
	var committed struct {
		RunID domain.RunID `json:"run_id"`
		State domain.State `json:"state"`
	}
	if receipt.RunID != id || current.StopReceiptID != receipt.ID || json.Unmarshal(receipt.ResponseProjection, &committed) != nil {
		return stopAcceptance{}, store.ErrCorruptStore
	}
	// A stop commits failed (queued run) or canceling (executing run).
	if committed.RunID != id || (committed.State != domain.Failed && committed.State != domain.Canceling) {
		return stopAcceptance{}, store.ErrCorruptStore
	}
	return stopAcceptance{id, committed.State, true}, nil
}

func (s *service) Stop(ctx context.Context, actor domain.ActorSnapshot, key domain.IdempotencyKey, id domain.RunID) (stopAcceptance, error) {
	var zero stopAcceptance
	claim, err := s.mutationClaim(actor, key, store.StopBackgroundRunCommand, id)
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
	result, err := s.config.Store.StopBackgroundRun(ctx, store.StopBackgroundRunParams{WorkspaceID: s.config.WorkspaceID,
		RunID: id, Claim: claim,
		APIContractVersion: APIContractVersion, StoppedAt: s.config.Now().UTC().Truncate(time.Millisecond)})
	if err != nil {
		return zero, err
	}
	if result.Replayed {
		return stopReplay(id, result.Run, result.Receipt)
	}
	s.notify(false)
	return stopAcceptance{id, domain.State(result.Run.State), false}, nil
}

func (s *service) Seal(ctx context.Context, actor domain.ActorSnapshot, key domain.IdempotencyKey, id domain.RunID) (sealAcceptance, error) {
	var zero sealAcceptance
	claim, err := s.mutationClaim(actor, key, store.SealBackgroundRunCommand, id)
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
	admission, err := s.config.Store.SealBackgroundRun(ctx, store.SealBackgroundRunParams{
		WorkspaceID: s.config.WorkspaceID, RunID: current.RunID, ExpectedRunRevision: current.Revision,
		ResultID: resultID, Claim: claim, PolicyVersion: s.config.SealPolicyVersion,
		APIContractVersion: APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		return zero, err
	}
	state, phase := domain.Canceling, "seal_requested"
	if admission.Run.State == domain.ResultReady {
		state, phase = domain.ResultReady, "ready"
	}
	s.notify(admission.Replayed)
	return sealAcceptance{admission.Run.RunID, state, phase, admission.Run.Seal.ResultID, true, admission.Replayed}, nil
}
