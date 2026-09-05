// Package resultapi exposes authenticated result reads and publication admission.
package resultapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	PathPrefix          = "/fern/api/v1/results/"
	APIContractVersion  = "fern.result.v1"
	maxPublishBodyBytes = 512
)

// Store is the persistence surface used by Handler.
type Store interface {
	GetResultOwners(context.Context, task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, error)
	HasRetainedResultAuthority(context.Context, task.ResultID) (bool, error)
	AdmitPublication(context.Context, taskstore.AdmitPublicationParams) (taskstore.PublicationAdmission, error)
}

var _ Store = (*taskstore.Store)(nil)

type ActorResolver func(context.Context) (task.ActorSnapshot, error)

// Config contains the workspace and server-owned publication policy.
type Config struct {
	WorkspaceID              task.WorkspaceID
	Store                    Store
	Generator                *task.Generator
	ActorResolver            ActorResolver
	Wake                     func()
	Now                      func() time.Time
	PublicationPolicyVersion string
	PublicationPolicySHA256  [sha256.Size]byte
	APIContractVersion       string
}

// Handler is safe for concurrent use when its injected dependencies are safe
// for concurrent use.
type Handler struct{ config Config }

func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Generator == nil || config.ActorResolver == nil || config.Wake == nil || config.Now == nil {
		return nil, errors.New("result API dependencies are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid result API workspace is required")
	}
	if !validText(config.PublicationPolicyVersion, 1, 128) || config.PublicationPolicySHA256 == ([sha256.Size]byte{}) ||
		!validText(config.APIContractVersion, 1, 64) {
		return nil, errors.New("valid result API publication policy is required")
	}
	return &Handler{config: config}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.EscapedPath() != r.URL.Path || !strings.HasPrefix(r.URL.Path, PathPrefix) {
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	actor, err := h.config.ActorResolver(r.Context())
	if err != nil || actor.Validate() != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		return
	}
	if actor.Type != task.ActorDevice && actor.Type != task.ActorOperator {
		writeError(w, http.StatusForbidden, "forbidden", "Access to this result API is forbidden.")
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, PathPrefix), "/")
	if len(parts) < 1 || len(parts) > 2 || (len(parts) == 2 && parts[1] != "publications") {
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	resultID, err := task.ParseResultID(parts[0])
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.get(w, r, resultID)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	h.publish(w, r, actor, resultID)
}

type resultDTO struct {
	ID              task.ResultID      `json:"id"`
	State           task.ResultState   `json:"state"`
	Outcome         task.ResultOutcome `json:"outcome"`
	BaseSHA         task.GitOID        `json:"baseSha"`
	ResultCommit    task.GitOID        `json:"resultCommit"`
	ManifestEntries int                `json:"manifestEntries"`
	ManifestSHA256  string             `json:"manifestSha256"`
	SealedAt        time.Time          `json:"sealedAt"`
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, id task.ResultID) {
	if !noQuery(r) || !noBody(r) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
		return
	}
	result, _, _, ok := h.ownedResult(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, resultDTO{
		ID: result.ID, State: result.State, Outcome: result.Outcome, BaseSHA: result.BaseSHA,
		ResultCommit: result.ResultCommit, ManifestEntries: result.ManifestEntries,
		ManifestSHA256: "sha256:" + hex.EncodeToString(result.ManifestSHA256[:]), SealedAt: result.SealedAt,
	})
}

type publicationInput struct {
	ExpectedVerificationID string
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, resultID task.ResultID) {
	if !noQuery(r) || !exactJSONContentType(r) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
		return
	}
	key, ok := parseIdempotencyKey(w, r)
	if !ok {
		return
	}
	var input publicationInput
	if err := decodeClosedObject(r.Body, maxPublishBodyBytes, map[string]func(json.RawMessage) error{
		"expectedVerificationId": stringField(&input.ExpectedVerificationID),
	}); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The JSON body is not valid.")
		return
	}
	verificationID, err := task.ParseVerificationID(input.ExpectedVerificationID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "The JSON body is not valid.")
		return
	}
	_, owner, _, ok := h.ownedResult(w, r, resultID)
	if !ok {
		return
	}
	authorized, err := h.config.Store.HasRetainedResultAuthority(r.Context(), resultID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !authorized {
		writeError(w, http.StatusConflict, "invalid_state", "The result does not have retained artifact authority.")
		return
	}
	ids, err := h.config.Generator.GeneratePublicationAdmissionIDs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	now := h.config.Now().UTC().Truncate(time.Millisecond)
	if now.IsZero() || now.UnixMilli() < 0 {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	claim := task.IdempotencyClaim{
		Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: taskstore.PublishResultCommand},
		Key:   key, RequestHash: publicationHash(resultID, verificationID), Actor: actor,
	}
	admission, err := h.config.Store.AdmitPublication(r.Context(), taskstore.AdmitPublicationParams{
		PublicationID: ids.PublicationID, OperationID: ids.OperationID, ReceiptID: ids.ReceiptID, EventID: ids.EventID,
		ResultID: resultID, VerificationID: verificationID, Claim: claim,
		BrokerPolicyVersion: h.config.PublicationPolicyVersion, BrokerPolicySHA256: h.config.PublicationPolicySHA256,
		APIContractVersion: h.config.APIContractVersion, AcceptedAt: now,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	response, valid := publicationResponseFor(admission, h.config.WorkspaceID, owner.ID)
	if !valid {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		h.config.Wake()
	}
	writeJSON(w, http.StatusAccepted, response)
}

func (h *Handler) ownedResult(w http.ResponseWriter, r *http.Request, id task.ResultID) (taskstore.Result, taskstore.Task, taskstore.Attempt, bool) {
	result, owner, attempt, err := h.config.Store.GetResultOwners(r.Context(), id)
	if err != nil {
		if errors.Is(err, taskstore.ErrNotFound) || errors.Is(err, taskstore.ErrInvalidState) {
			writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
			return taskstore.Result{}, taskstore.Task{}, taskstore.Attempt{}, false
		}
		writeStoreError(w, err)
		return taskstore.Result{}, taskstore.Task{}, taskstore.Attempt{}, false
	}
	if result.ID != id || result.WorkspaceID != h.config.WorkspaceID || owner.WorkspaceID != h.config.WorkspaceID || attempt.WorkspaceID != h.config.WorkspaceID {
		writeError(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
		return taskstore.Result{}, taskstore.Task{}, taskstore.Attempt{}, false
	}
	if result.TaskID != owner.ID || result.AttemptID != attempt.ID || owner.CurrentAttemptID != attempt.ID ||
		owner.SealedResultID != result.ID || attempt.TaskID != owner.ID || attempt.SealedResultID != result.ID {
		writeError(w, http.StatusInternalServerError, "internal_error", "The request could not be completed.")
		return taskstore.Result{}, taskstore.Task{}, taskstore.Attempt{}, false
	}
	return result, owner, attempt, true
}

type receiptDTO struct {
	ID         task.ReceiptID `json:"id"`
	Kind       string         `json:"kind"`
	State      string         `json:"state"`
	AcceptedAt time.Time      `json:"acceptedAt"`
	TargetID   task.TaskID    `json:"targetId"`
}

type publicationAdmissionDTO struct {
	ID             task.PublicationID         `json:"id"`
	ResultID       task.ResultID              `json:"resultId"`
	VerificationID task.VerificationID        `json:"verificationId"`
	State          taskstore.PublicationState `json:"state"`
	CreatedAt      time.Time                  `json:"createdAt"`
}

type publicationResponse struct {
	Receipt     receiptDTO              `json:"receipt"`
	Publication publicationAdmissionDTO `json:"publication"`
}

func publicationResponseFor(value taskstore.PublicationAdmission, workspaceID task.WorkspaceID, ownerID task.TaskID) (publicationResponse, bool) {
	var projection struct {
		PublicationID  task.PublicationID  `json:"publicationId"`
		ResultID       task.ResultID       `json:"resultId"`
		VerificationID task.VerificationID `json:"verificationId"`
	}
	if json.Unmarshal(value.Receipt.ResponseProjection, &projection) != nil {
		return publicationResponse{}, false
	}
	if _, err := task.ParsePublicationID(string(projection.PublicationID)); err != nil {
		return publicationResponse{}, false
	}
	if _, err := task.ParseResultID(string(projection.ResultID)); err != nil {
		return publicationResponse{}, false
	}
	if _, err := task.ParseVerificationID(string(projection.VerificationID)); err != nil {
		return publicationResponse{}, false
	}
	if value.Receipt.WorkspaceID != workspaceID || value.Receipt.CommandKind != taskstore.PublishResultCommand ||
		value.Receipt.State != taskstore.ReceiptAccepted || value.Receipt.TargetID != ownerID || value.Receipt.AcceptedAt.IsZero() ||
		value.Publication.WorkspaceID != workspaceID || value.Publication.TaskID != ownerID ||
		projection.PublicationID != value.Publication.ID || projection.ResultID != value.Publication.ResultID ||
		projection.VerificationID != value.Publication.VerificationID {
		return publicationResponse{}, false
	}
	return publicationResponse{
		Receipt: receiptDTO{value.Receipt.ID, value.Receipt.CommandKind, value.Receipt.State, value.Receipt.AcceptedAt, value.Receipt.TargetID},
		Publication: publicationAdmissionDTO{projection.PublicationID, projection.ResultID, projection.VerificationID,
			taskstore.PublicationPrepared, value.Receipt.AcceptedAt},
	}, true
}

func publicationHash(resultID task.ResultID, verificationID task.VerificationID) task.RequestHash {
	projection := struct {
		ResultID               task.ResultID       `json:"resultId"`
		ExpectedVerificationID task.VerificationID `json:"expectedVerificationId"`
	}{resultID, verificationID}
	canonical, err := json.Marshal(projection)
	if err != nil {
		panic("resultapi: fixed canonical projection cannot fail: " + err.Error())
	}
	return task.RequestHash(sha256.Sum256(append(append([]byte(taskstore.PublishResultCommand), '\n'), canonical...)))
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
func noBody(r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	defer func() { _ = r.Body.Close() }()
	value, err := io.ReadAll(io.LimitReader(r.Body, 1))
	return err == nil && len(value) == 0
}

func validText(value string, min, max int) bool {
	if len(value) < min || len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
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

func decodeClosedObject(body io.ReadCloser, maxBytes int64, fields map[string]func(json.RawMessage) error) error {
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes || !utf8.Valid(data) {
		return errors.New("invalid or oversized body")
	}
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
			return errors.New("unknown field")
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
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed for this resource.")
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
	case errors.Is(err, taskstore.ErrInvalidState):
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
