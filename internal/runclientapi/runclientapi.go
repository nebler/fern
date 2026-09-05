// Package runclientapi exposes terminal-client run discovery and attachment.
// It is deliberately separate from the plugin-only run submission API.
package runclientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const PathPrefix = "/fern/api/v1/runs"

type Store interface {
	GetBackgroundRun(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRun, error)
	ListBackgroundRuns(context.Context, task.WorkspaceID, task.ActorSnapshot, int) ([]taskstore.BackgroundRun, error)
}

type Route interface {
	IssueAttachment(taskstore.BackgroundRun) (backgroundroute.Attachment, bool, error)
	ActiveOrigin(taskstore.BackgroundRun) (string, bool)
}

type Config struct {
	WorkspaceID task.WorkspaceID
	Store       Store
	Route       Route
}

type Handler struct{ config Config }

func New(config Config) (*Handler, error) {
	if config.Store == nil || config.Route == nil {
		return nil, errors.New("run client store and route are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid run client workspace is required")
	}
	return &Handler{config: config}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request.URL.EscapedPath() != request.URL.Path || request.Method != http.MethodGet || request.URL.RawQuery != "" || !emptyBody(request) {
		writeError(writer, http.StatusNotFound, "not_found", "The requested run client operation was not found.")
		return
	}
	actor, err := task.ContextActor(request.Context())
	if err != nil || !clientActor(actor) {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "Run client authentication is required.")
		return
	}
	if request.URL.Path == PathPrefix {
		if !authorized(writer, request, actor, "run:read") {
			return
		}
		handler.list(writer, request, actor)
		return
	}
	suffix, found := strings.CutPrefix(request.URL.Path, PathPrefix+"/")
	parts := strings.Split(suffix, "/")
	if !found || len(parts) != 2 || parts[1] != "attach" {
		writeError(writer, http.StatusNotFound, "not_found", "The requested run client operation was not found.")
		return
	}
	runID, err := task.ParseTaskID(parts[0])
	if err != nil || !authorized(writer, request, actor, "run:attach") {
		if err != nil {
			writeError(writer, http.StatusNotFound, "not_found", "The requested run was not found.")
		}
		return
	}
	handler.attach(writer, request, actor, runID)
}

func clientActor(actor task.ActorSnapshot) bool {
	return actor.Type == task.ActorOpenCode || actor.Type == task.ActorOperator || actor.Type == task.ActorDevice
}

func authorized(writer http.ResponseWriter, request *http.Request, actor task.ActorSnapshot, scope string) bool {
	if actor.Type == task.ActorOperator || actor.Type == task.ActorDevice {
		return true
	}
	authorization, ok := pluginauth.RequestAuthorizationFromContext(request.Context())
	if !ok || actor.ID != authorization.Credential.ID || actor.CredentialID != authorization.Credential.ID ||
		actor.Authentication != "fern_plugin_bearer" || !authorization.HasScope(scope) {
		writeError(writer, http.StatusForbidden, "forbidden", "The client credential lacks the required scope.")
		return false
	}
	return true
}

type runProjection struct {
	ID         task.TaskID                  `json:"id"`
	State      taskstore.BackgroundRunState `json:"state"`
	Repository string                       `json:"repository"`
	Head       task.GitOID                  `json:"head"`
	Branch     *string                      `json:"branch"`
	Attachable bool                         `json:"attachable"`
}

func (handler *Handler) list(writer http.ResponseWriter, request *http.Request, actor task.ActorSnapshot) {
	runs, err := handler.config.Store.ListBackgroundRuns(request.Context(), handler.config.WorkspaceID, actor, taskstore.MaxBackgroundRunListLimit)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	projection := make([]runProjection, 0, len(runs))
	for _, run := range runs {
		_, attachable := handler.config.Route.ActiveOrigin(run)
		attachable = attachable && attachmentReady(run)
		projection = append(projection, runProjection{ID: run.TaskID, State: run.State, Repository: run.RepositoryRemote,
			Head: run.BaseOID, Branch: run.Branch, Attachable: attachable})
	}
	writeJSON(writer, http.StatusOK, struct {
		Runs []runProjection `json:"runs"`
	}{projection})
}

type attachProjection struct {
	RunID     task.TaskID            `json:"run_id"`
	URL       string                 `json:"url"`
	SessionID task.OpenCodeSessionID `json:"session_id"`
	Username  string                 `json:"username"`
	Password  string                 `json:"password"`
	ExpiresAt time.Time              `json:"expires_at"`
}

func (handler *Handler) attach(writer http.ResponseWriter, request *http.Request, actor task.ActorSnapshot, runID task.TaskID) {
	run, err := handler.config.Store.GetBackgroundRun(request.Context(), handler.config.WorkspaceID, runID, actor)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	if !attachmentReady(run) {
		writeError(writer, http.StatusConflict, "not_ready", "The OpenCode session is not ready for attachment.")
		return
	}
	attachment, issued, err := handler.config.Route.IssueAttachment(run)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "unavailable", "The OpenCode attachment could not be issued.")
		return
	}
	if !issued {
		writeError(writer, http.StatusConflict, "not_ready", "The OpenCode session is not ready for attachment.")
		return
	}
	writeJSON(writer, http.StatusOK, attachProjection{RunID: run.TaskID, URL: attachment.Origin, SessionID: run.OpenCodeSessionID,
		Username: attachment.Username, Password: attachment.Password, ExpiresAt: attachment.ExpiresAt})
}

func attachmentReady(run taskstore.BackgroundRun) bool {
	active := run.State == taskstore.BackgroundRunSettingUp || run.State == taskstore.BackgroundRunWorking ||
		run.State == taskstore.BackgroundRunNeedsYou || run.State == taskstore.BackgroundRunUncertain
	ready := run.EffectPhase == taskstore.BackgroundRunEffectSessionObserved || run.EffectPhase == taskstore.BackgroundRunEffectPromptIntent ||
		run.EffectPhase == taskstore.BackgroundRunEffectPromptAdmitted
	return active && ready && run.SessionObservedAt != nil && run.CancelEpoch == 0
}

func emptyBody(request *http.Request) bool {
	if request.Body == nil || request.Body == http.NoBody {
		return true
	}
	buffer := make([]byte, 1)
	count, err := request.Body.Read(buffer)
	return count == 0 && errors.Is(err, io.EOF)
}

func writeStoreError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, taskstore.ErrNotFound):
		writeError(writer, http.StatusNotFound, "not_found", "The requested run was not found.")
	default:
		writeError(writer, http.StatusInternalServerError, "internal_error", "The run client operation failed.")
	}
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
