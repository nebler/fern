// Package taskapi exposes the authenticated HTTP boundary for durable tasks.
package taskapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	submitPath         = "/fern/api/v1/tasks"
	eventsPath         = "/fern/api/v1/events"
	apiPrefix          = "/fern/api/v1/tasks/"
	maxPromptBytes     = 64 * 1024
	maxTitleBytes      = 200
	maxBaseRefBytes    = 255
	maxReasonBytes     = 500
	maxSubmitBodyBytes = 6*(maxPromptBytes+maxTitleBytes+maxBaseRefBytes) + 128
	maxCancelBodyBytes = 6*maxReasonBytes + 32
	defaultEventLimit  = 100
	maxEventLimit      = 500
)

var (
	// ErrUnauthenticated is returned by ActorResolver when no authenticated
	// principal is present in the server-owned request context.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden is returned by ActorResolver when the principal may not use
	// this workspace API.
	ErrForbidden = errors.New("forbidden")
)

// Store is the complete persistence surface used by Handler.
type Store interface {
	AdmitTask(context.Context, taskstore.AdmitTaskParams) (taskstore.Admission, error)
	FindReceiptByIdempotency(context.Context, task.WorkspaceID, string, task.IdempotencyKey) (taskstore.Receipt, bool, error)
	GetTask(context.Context, task.TaskID) (taskstore.Task, error)
	GetAttempt(context.Context, task.AttemptID) (taskstore.Attempt, error)
	RequestCancellation(context.Context, taskstore.RequestCancellationParams) (taskstore.Cancellation, error)
	ListEvents(context.Context, task.WorkspaceID, task.Cursor, int) (taskstore.EventPage, error)
}

var _ Store = (*taskstore.Store)(nil)

// ActorResolver resolves authentication established by the outer server. It is
// deliberately given only a context, so this package cannot derive identity
// from client-controlled headers or bodies.
type ActorResolver func(context.Context) (task.ActorSnapshot, error)

type actorContextKey struct{}

// WithActor installs an actor already authenticated by outer ingress
// middleware. HTTP headers and bodies are intentionally not consulted.
func WithActor(ctx context.Context, actor task.ActorSnapshot) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ContextActor is the default ActorResolver for authenticated proxy wiring.
func ContextActor(ctx context.Context) (task.ActorSnapshot, error) {
	actor, ok := ctx.Value(actorContextKey{}).(task.ActorSnapshot)
	if !ok {
		return task.ActorSnapshot{}, ErrUnauthenticated
	}
	if err := actor.Validate(); err != nil {
		return task.ActorSnapshot{}, ErrUnauthenticated
	}
	return actor, nil
}

// BaseResolver resolves a display ref to the authoritative commit for the
// configured repository and workspace.
type BaseResolver func(context.Context, string) (task.GitOID, error)

// Config contains dependencies and all execution values clients are forbidden
// from selecting. Wake must be nonblocking; Handler calls it after a fresh
// command commits and never for an idempotency replay.
type Config struct {
	WorkspaceID              task.WorkspaceID
	RepositoryID             task.RepositoryID
	Store                    Store
	Generator                *task.Generator
	ActorResolver            ActorResolver
	BaseResolver             BaseResolver
	Wake                     func()
	Now                      func() time.Time
	AttemptTimeout           time.Duration
	ObjectFormat             string
	APIContractVersion       string
	ExecutionContractVersion string
	Agent                    string
	ModelProvider            string
	Model                    string
	BudgetSnapshot           json.RawMessage
}

// Handler is safe for concurrent use when its injected dependencies are safe
// for concurrent use.
type Handler struct {
	config Config
}

// New constructs a task API handler and rejects incomplete server policy.
func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Generator == nil || config.ActorResolver == nil || config.BaseResolver == nil || config.Wake == nil || config.Now == nil {
		return nil, errors.New("task API dependencies are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid task API workspace is required")
	}
	if config.RepositoryID == 0 || uint64(config.RepositoryID) > math.MaxInt64 {
		return nil, errors.New("valid task API repository is required")
	}
	if config.AttemptTimeout <= 0 {
		return nil, errors.New("positive task API attempt timeout is required")
	}
	if config.ObjectFormat != "sha1" || !validText(config.APIContractVersion, 1, 64) ||
		!validText(config.ExecutionContractVersion, 1, 128) || !validText(config.Agent, 1, 128) ||
		!validText(config.ModelProvider, 1, 128) || !validText(config.Model, 1, 256) {
		return nil, errors.New("valid task API execution policy is required")
	}
	if len(config.BudgetSnapshot) == 0 || len(config.BudgetSnapshot) > 16*1024 || !json.Valid(config.BudgetSnapshot) {
		return nil, errors.New("valid task API budget snapshot is required")
	}
	config.BudgetSnapshot = append(json.RawMessage(nil), config.BudgetSnapshot...)
	return &Handler{config: config}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.EscapedPath() != r.URL.Path {
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	actor, err := h.config.ActorResolver(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, ErrForbidden):
			writeError(w, http.StatusForbidden, "forbidden", "Access to this workspace is forbidden.")
		case errors.Is(err, ErrUnauthenticated):
			writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		}
		return
	}
	if err := actor.Validate(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}

	switch {
	case r.URL.Path == submitPath:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.submit(w, r, actor)
	case r.URL.Path == eventsPath:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.events(w, r)
	case strings.HasPrefix(r.URL.Path, apiPrefix):
		h.taskRoute(w, r, actor)
	default:
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
	}
}

type submitInput struct {
	Title   string
	Prompt  string
	BaseRef string
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	if !noQuery(r) || !exactJSONContentType(r) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
		return
	}
	key, ok := parseIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input submitInput
	err := decodeClosedObject(r.Body, maxSubmitBodyBytes, map[string]func(json.RawMessage) error{
		"title":   stringField(&input.Title),
		"prompt":  stringField(&input.Prompt),
		"baseRef": stringField(&input.BaseRef),
	})
	if err != nil || !validText(input.Title, 1, maxTitleBytes) || len(input.Prompt) < 1 || len(input.Prompt) > maxPromptBytes || !utf8.ValidString(input.Prompt) || !validText(input.BaseRef, 1, maxBaseRefBytes) {
		writeError(w, http.StatusBadRequest, "invalid_json", "The JSON body is not valid.")
		return
	}
	claim := task.IdempotencyClaim{
		Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: taskstore.SubmitTaskCommand},
		Key:   key, RequestHash: submitHash(input), Actor: actor,
	}
	receipt, found, err := h.config.Store.FindReceiptByIdempotency(r.Context(), h.config.WorkspaceID, taskstore.SubmitTaskCommand, key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if found {
		h.replaySubmission(w, r, receipt, claim)
		return
	}

	baseSHA, err := h.config.BaseResolver(r.Context(), input.BaseRef)
	if err != nil {
		writeDependencyError(w, r.Context(), "base_unavailable", "The base ref could not be resolved.", err)
		return
	}
	if _, err := task.ParseGitOID(string(baseSHA)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	ids, err := h.config.Generator.GenerateAdmissionIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	now, deadline, ok := h.commandTimes(w)
	if !ok {
		return
	}
	result, err := h.config.Store.AdmitTask(r.Context(), taskstore.AdmitTaskParams{
		TaskID: ids.TaskID, AttemptID: ids.AttemptID, ReceiptID: ids.ReceiptID,
		TaskEventID: ids.TaskEventID, AttemptEventID: ids.AttemptEventID,
		OpenCodeSessionID: ids.OpenCodeSessionID, OpenCodeMessageID: ids.OpenCodeMessageID,
		Claim: claim, Title: input.Title, Prompt: input.Prompt, RepositoryID: h.config.RepositoryID,
		BaseRef: input.BaseRef, BaseSHA: baseSHA, ObjectFormat: h.config.ObjectFormat,
		ExecutionContractVersion: h.config.ExecutionContractVersion, Agent: h.config.Agent,
		ModelProvider: h.config.ModelProvider, Model: h.config.Model,
		BudgetSnapshot: append(json.RawMessage(nil), h.config.BudgetSnapshot...), Deadline: deadline,
		APIContractVersion: h.config.APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	response := submitResponseFor(result, h.config.WorkspaceID, h.config.RepositoryID)
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		h.config.Wake()
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (h *Handler) replaySubmission(w http.ResponseWriter, r *http.Request, receipt taskstore.Receipt, claim task.IdempotencyClaim) {
	owner, err := h.config.Store.GetTask(r.Context(), receipt.TargetID)
	if err != nil || owner.WorkspaceID != h.config.WorkspaceID {
		writeStoreError(w, coalesceError(err, taskstore.ErrCorruptStore))
		return
	}
	attempt, err := h.config.Store.GetAttempt(r.Context(), owner.CurrentAttemptID)
	if err != nil || attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID {
		writeStoreError(w, coalesceError(err, taskstore.ErrCorruptStore))
		return
	}
	taskEventID, err := h.config.Generator.EventID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	attemptEventID, err := h.config.Generator.EventID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	result, err := h.config.Store.AdmitTask(r.Context(), taskstore.AdmitTaskParams{
		TaskID: owner.ID, AttemptID: attempt.ID, ReceiptID: receipt.ID,
		TaskEventID: taskEventID, AttemptEventID: attemptEventID,
		OpenCodeSessionID: attempt.OpenCodeSessionID, OpenCodeMessageID: attempt.OpenCodeMessageID,
		Claim: claim, Title: owner.Title, Prompt: owner.Prompt, RepositoryID: owner.RepositoryID,
		BaseRef: owner.BaseRef, BaseSHA: owner.BaseSHA, ObjectFormat: owner.ObjectFormat,
		ExecutionContractVersion: attempt.ExecutionContractVersion, Agent: attempt.Agent,
		ModelProvider: attempt.ModelProvider, Model: attempt.Model,
		BudgetSnapshot: append(json.RawMessage(nil), attempt.BudgetSnapshot...), Deadline: attempt.Deadline,
		APIContractVersion: receipt.APIContractVersion, AcceptedAt: receipt.AcceptedAt,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Idempotency-Replayed", "true")
	writeJSON(w, http.StatusAccepted, submitResponseFor(result, h.config.WorkspaceID, h.config.RepositoryID))
}

func (h *Handler) taskRoute(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	remainder := strings.TrimPrefix(r.URL.Path, apiPrefix)
	parts := strings.Split(remainder, "/")
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		id, err := task.ParseTaskID(parts[0])
		if err != nil || !noQuery(r) {
			writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
			return
		}
		h.getTask(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		id, err := task.ParseTaskID(parts[0])
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
			return
		}
		h.cancel(w, r, actor, id)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request, id task.TaskID) {
	owner, err := h.config.Store.GetTask(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if owner.WorkspaceID != h.config.WorkspaceID {
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	attempt, err := h.config.Store.GetAttempt(r.Context(), owner.CurrentAttemptID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if attempt.WorkspaceID != h.config.WorkspaceID || attempt.TaskID != owner.ID || attempt.ID != owner.CurrentAttemptID {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Task    taskDTO    `json:"task"`
		Attempt attemptDTO `json:"attempt"`
	}{taskView(owner), attemptView(attempt)})
}

type cancelInput struct {
	Reason string
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	if !noQuery(r) || !exactJSONContentType(r) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
		return
	}
	key, ok := parseIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input cancelInput
	if err := decodeOptionalClosedObject(r.Body, maxCancelBodyBytes, map[string]func(json.RawMessage) error{
		"reason": stringField(&input.Reason),
	}); err != nil || (input.Reason != "" && !validText(input.Reason, 1, maxReasonBytes)) {
		writeError(w, http.StatusBadRequest, "invalid_json", "The JSON body is not valid.")
		return
	}
	receiptID, err := h.config.Generator.ReceiptID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	attemptEventID, err := h.config.Generator.EventID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	taskEventID, err := h.config.Generator.EventID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	now, _, ok := h.commandTimes(w)
	if !ok {
		return
	}
	claim := task.IdempotencyClaim{
		Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: taskstore.CancelTaskCommand},
		Key:   key, RequestHash: cancelHash(id, input), Actor: actor,
	}
	result, err := h.config.Store.RequestCancellation(r.Context(), taskstore.RequestCancellationParams{
		TaskID: id, ReceiptID: receiptID, AttemptEventID: attemptEventID, TaskEventID: taskEventID,
		Claim: claim, Reason: input.Reason, Now: now, APIContractVersion: h.config.APIContractVersion,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	response := cancelResponseFor(result)
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		h.config.Wake()
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	for key, entries := range values {
		if (key != "after" && key != "limit") || len(entries) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "The event query is not valid.")
			return
		}
	}
	after, err := task.ParseAfterCursor(values.Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "The event query is not valid.")
		return
	}
	limit := defaultEventLimit
	if raw := values.Get("limit"); raw != "" {
		if len(raw) > 1 && raw[0] == '0' {
			writeError(w, http.StatusBadRequest, "invalid_query", "The event query is not valid.")
			return
		}
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxEventLimit {
			writeError(w, http.StatusBadRequest, "invalid_query", "The event query is not valid.")
			return
		}
	}
	page, err := h.config.Store.ListEvents(r.Context(), h.config.WorkspaceID, after, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	events := make([]eventDTO, 0, len(page.Events))
	if page.NextCursor.Validate() != nil || page.Watermark.Validate() != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	for _, event := range page.Events {
		if event.WorkspaceID != h.config.WorkspaceID || event.Cursor.ValidateEvent() != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
			return
		}
		events = append(events, eventView(event))
	}
	writeJSON(w, http.StatusOK, struct {
		Events     []eventDTO  `json:"events"`
		NextCursor task.Cursor `json:"nextCursor"`
		Watermark  task.Cursor `json:"watermark"`
		CaughtUp   bool        `json:"caughtUp"`
	}{events, page.NextCursor, page.Watermark, page.CaughtUp})
}

func (h *Handler) commandTimes(w http.ResponseWriter) (time.Time, time.Time, bool) {
	now := h.config.Now().UTC().Truncate(time.Millisecond)
	deadline := now.Add(h.config.AttemptTimeout)
	if now.IsZero() || now.UnixMilli() < 0 || !deadline.After(now) || deadline.UnixMilli() < 0 {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return time.Time{}, time.Time{}, false
	}
	return now, deadline, true
}

func submitHash(input submitInput) task.RequestHash {
	projection := struct {
		Title   string `json:"title"`
		Prompt  string `json:"prompt"`
		BaseRef string `json:"baseRef"`
	}{input.Title, input.Prompt, input.BaseRef}
	return commandHash(taskstore.SubmitTaskCommand, projection)
}

func cancelHash(id task.TaskID, input cancelInput) task.RequestHash {
	projection := struct {
		TaskID task.TaskID `json:"taskId"`
		Reason string      `json:"reason"`
	}{id, input.Reason}
	return commandHash(taskstore.CancelTaskCommand, projection)
}

func commandHash(kind string, projection any) task.RequestHash {
	canonical, err := json.Marshal(projection)
	if err != nil {
		panic("taskapi: fixed canonical projection cannot fail: " + err.Error())
	}
	return task.RequestHash(sha256.Sum256(append(append([]byte(kind), '\n'), canonical...)))
}

func parseIdempotencyKey(w http.ResponseWriter, r *http.Request) (task.IdempotencyKey, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "A valid Idempotency-Key is required.")
		return "", false
	}
	key, err := task.ParseIdempotencyKey(values[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "A valid Idempotency-Key is required.")
		return "", false
	}
	return key, true
}

func exactJSONContentType(r *http.Request) bool {
	return r.Header.Get("Content-Type") == "application/json" && len(r.Header.Values("Content-Type")) == 1
}

func noQuery(r *http.Request) bool { return r.URL.RawQuery == "" }

func validText(value string, min, max int) bool {
	if len(value) < min || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func stringField(target *string) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		if !validUnicodeEscapes(raw) {
			return errors.New("invalid Unicode escape")
		}
		return json.Unmarshal(raw, target)
	}
}

func validUnicodeEscapes(raw []byte) bool {
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			i++
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			i++
			continue
		}
		value, ok := hexQuad(raw, i+1)
		if !ok {
			return false
		}
		i += 5
		switch {
		case value >= 0xd800 && value <= 0xdbff:
			if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
				return false
			}
			low, ok := hexQuad(raw, i+2)
			if !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		case value >= 0xdc00 && value <= 0xdfff:
			return false
		}
	}
	return true
}

func hexQuad(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, char := range raw[start : start+4] {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value |= uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value |= uint16(char-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func decodeOptionalClosedObject(body io.ReadCloser, maxBytes int64, fields map[string]func(json.RawMessage) error) error {
	data, err := readBounded(body, maxBytes)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return decodeClosedObjectBytes(data, fields)
}

func decodeClosedObject(body io.ReadCloser, maxBytes int64, fields map[string]func(json.RawMessage) error) error {
	data, err := readBounded(body, maxBytes)
	if err != nil {
		return err
	}
	return decodeClosedObjectBytes(data, fields)
}

func readBounded(body io.ReadCloser, maxBytes int64) ([]byte, error) {
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes || !utf8.Valid(data) {
		return nil, errors.New("invalid or oversized body")
	}
	return data, nil
}

func decodeClosedObjectBytes(data []byte, fields map[string]func(json.RawMessage) error) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("body must be an object")
	}
	seen := make(map[string]struct{}, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("object key must be a string")
		}
		decode, allowed := fields[name]
		if !allowed {
			return errors.New("unknown or case-aliased field")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("duplicate field")
		}
		seen[name] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if err := decode(raw); err != nil {
			return err
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("unterminated object")
	}
	if token, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

type receiptDTO struct {
	ID         task.ReceiptID `json:"id"`
	Kind       string         `json:"kind"`
	State      string         `json:"state"`
	AcceptedAt time.Time      `json:"acceptedAt"`
	TargetID   task.TaskID    `json:"targetId"`
}

type taskDTO struct {
	ID                      task.TaskID      `json:"id"`
	WorkspaceID             task.WorkspaceID `json:"workspaceId"`
	Title                   string           `json:"title"`
	State                   task.TaskState   `json:"state"`
	RepositoryID            string           `json:"repositoryId"`
	BaseRef                 string           `json:"baseRef"`
	BaseSHA                 task.GitOID      `json:"baseSha"`
	CurrentAttemptID        task.AttemptID   `json:"currentAttemptId"`
	CancelEpoch             uint64           `json:"cancelEpoch"`
	CancellationReason      *string          `json:"cancellationReason,omitempty"`
	CancellationRequestedAt *time.Time       `json:"cancellationRequestedAt,omitempty"`
	LatestEventCursor       task.Cursor      `json:"latestEventCursor"`
	Revision                int64            `json:"revision"`
	CreatedAt               time.Time        `json:"createdAt"`
	UpdatedAt               time.Time        `json:"updatedAt"`
}

type attemptDTO struct {
	ID           task.AttemptID    `json:"id"`
	TaskID       task.TaskID       `json:"taskId"`
	OpenCodePath string            `json:"openCodePath"`
	Sequence     int64             `json:"sequence"`
	State        task.AttemptState `json:"state"`
	Deadline     time.Time         `json:"deadline"`
	AdmittedAt   *time.Time        `json:"admittedAt,omitempty"`
	Revision     int64             `json:"revision"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

type eventDTO struct {
	ID         task.EventID   `json:"id"`
	Cursor     task.Cursor    `json:"cursor"`
	TaskID     task.TaskID    `json:"taskId,omitempty"`
	AttemptID  task.AttemptID `json:"attemptId,omitempty"`
	Type       string         `json:"type"`
	Version    int            `json:"version"`
	OccurredAt time.Time      `json:"occurredAt"`
}

func taskView(value taskstore.Task) taskDTO {
	return taskDTO{
		ID: value.ID, WorkspaceID: value.WorkspaceID, Title: value.Title, State: value.State,
		RepositoryID: strconv.FormatUint(uint64(value.RepositoryID), 10), BaseRef: value.BaseRef, BaseSHA: value.BaseSHA,
		CurrentAttemptID: value.CurrentAttemptID, CancelEpoch: value.CancelEpoch,
		CancellationReason: value.CancellationReason, CancellationRequestedAt: value.CancellationRequestedAt,
		LatestEventCursor: value.LatestEventCursor, Revision: value.Revision, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func attemptView(value taskstore.Attempt) attemptDTO {
	return attemptDTO{
		ID: value.ID, TaskID: value.TaskID, OpenCodePath: "/session/" + url.PathEscape(string(value.OpenCodeSessionID)),
		Sequence: value.Sequence, State: value.State, Deadline: value.Deadline,
		AdmittedAt: value.AdmittedAt, Revision: value.Revision, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func eventView(value taskstore.Event) eventDTO {
	return eventDTO{value.ID, value.Cursor, value.TaskID, value.AttemptID, value.Type, value.Version, value.OccurredAt}
}

type submitResponse struct {
	Receipt receiptDTO `json:"receipt"`
	Task    taskDTO    `json:"task"`
}

func submitResponseFor(value taskstore.Admission, workspaceID task.WorkspaceID, repositoryID task.RepositoryID) submitResponse {
	taskValue := taskView(value.Task)
	// Acceptance responses are receipt projections, not mutable snapshots. This
	// keeps an eventual replay byte-stable even if workers have advanced the task.
	taskValue.WorkspaceID = workspaceID
	taskValue.RepositoryID = strconv.FormatUint(uint64(repositoryID), 10)
	taskValue.State = task.TaskQueued
	taskValue.CancelEpoch = 0
	taskValue.CancellationReason = nil
	taskValue.CancellationRequestedAt = nil
	taskValue.CurrentAttemptID = admissionAttemptID(value)
	taskValue.LatestEventCursor = value.AttemptEvent.Cursor
	taskValue.Revision = 1
	taskValue.CreatedAt = value.Receipt.AcceptedAt
	taskValue.UpdatedAt = value.Receipt.AcceptedAt
	return submitResponse{receiptView(value.Receipt), taskValue}
}

func admissionAttemptID(value taskstore.Admission) task.AttemptID {
	var projection struct {
		AttemptID task.AttemptID `json:"attemptId"`
	}
	if json.Unmarshal(value.Receipt.ResponseProjection, &projection) == nil && projection.AttemptID != "" {
		return projection.AttemptID
	}
	return value.Attempt.ID
}

type cancelResponse struct {
	Receipt     receiptDTO                              `json:"receipt"`
	TaskID      task.TaskID                             `json:"taskId"`
	AttemptID   task.AttemptID                          `json:"attemptId"`
	CancelEpoch uint64                                  `json:"cancelEpoch"`
	Disposition taskstore.CancellationEffectDisposition `json:"effectDisposition"`
}

func cancelResponseFor(value taskstore.Cancellation) cancelResponse {
	var projection struct {
		AttemptID   task.AttemptID                          `json:"attemptId"`
		CancelEpoch uint64                                  `json:"cancelEpoch"`
		Disposition taskstore.CancellationEffectDisposition `json:"effectDisposition"`
	}
	_ = json.Unmarshal(value.Receipt.ResponseProjection, &projection)
	if projection.AttemptID == "" {
		projection.AttemptID = value.Attempt.ID
		projection.CancelEpoch = value.Task.CancelEpoch
		projection.Disposition = value.Disposition
	}
	return cancelResponse{receiptView(value.Receipt), value.Receipt.TargetID, projection.AttemptID, projection.CancelEpoch, projection.Disposition}
}

func receiptView(value taskstore.Receipt) receiptDTO {
	return receiptDTO{value.ID, value.CommandKind, value.State, value.AcceptedAt, value.TargetID}
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this resource.")
}

func writeDependencyError(w http.ResponseWriter, ctx context.Context, code, message string, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "deadline_exceeded", "The request deadline was exceeded.")
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		writeError(w, http.StatusRequestTimeout, "request_canceled", "The request was canceled.")
	default:
		writeError(w, http.StatusUnprocessableEntity, code, message)
	}
}

func coalesceError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "deadline_exceeded", "The request deadline was exceeded.")
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusRequestTimeout, "request_canceled", "The request was canceled.")
	case errors.Is(err, taskstore.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
	case errors.Is(err, taskstore.ErrIdempotencyOwnerMismatch):
		writeError(w, http.StatusForbidden, "idempotency_owner_mismatch", "The idempotency key is owned by another principal.")
	case errors.Is(err, taskstore.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "The idempotency key was already used for different input.")
	case errors.Is(err, taskstore.ErrCancellationAlreadyRequested):
		writeError(w, http.StatusConflict, "cancellation_already_requested", "Cancellation was already requested.")
	case errors.Is(err, taskstore.ErrTaskAlreadyTerminal):
		writeError(w, http.StatusConflict, "task_already_terminal", "The task is already terminal.")
	case errors.Is(err, taskstore.ErrRepositoryMismatch):
		writeError(w, http.StatusConflict, "repository_mismatch", "The workspace repository does not match.")
	case errors.Is(err, taskstore.ErrWorkspaceBusy), errors.Is(err, taskstore.ErrInvalidState):
		writeError(w, http.StatusConflict, "invalid_state", "The command is not valid in the current state.")
	case errors.Is(err, taskstore.ErrWorkspaceUnavailable):
		writeError(w, http.StatusServiceUnavailable, "workspace_unavailable", "The workspace is unavailable.")
	case errors.Is(err, taskstore.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}
