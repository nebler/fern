// Package runcontrol exposes paired-device and operator intervention for
// active Background Runs. The plugin-only runapi remains a separate boundary.
package runcontrol

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nebler/fern/internal/backgroundruncoord"
	"github.com/nebler/fern/internal/jsoncanon"
	"github.com/nebler/fern/internal/runterminal"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	APIPathPrefix      = "/fern/api/v1/runs"
	HTMLPathPrefix     = "/fern/runs"
	APIContractVersion = "fern.run-control.v1"
	maxBodyBytes       = 16 << 10
	maxInstruction     = 4000
)

type Store interface {
	GetBackgroundRunForControl(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRun, taskstore.BackgroundRunOwnership, error)
	ListBackgroundRunsForControl(context.Context, task.WorkspaceID, task.ActorSnapshot, int) ([]taskstore.BackgroundRunControlView, error)
	RequestBackgroundRunTakeover(context.Context, taskstore.RequestBackgroundRunTakeoverParams) (taskstore.BackgroundRunOwnershipAdmission, error)
	RequestBackgroundRunHandback(context.Context, taskstore.RequestBackgroundRunHandbackParams) (taskstore.BackgroundRunOwnershipAdmission, error)
	AdmitBackgroundRunControl(context.Context, taskstore.AdmitBackgroundRunControlParams) (taskstore.BackgroundRunControlAdmission, error)
	LatestBackgroundRunControl(context.Context, task.WorkspaceID, task.TaskID) (taskstore.BackgroundRunControl, error)
}

type Controller interface {
	ObserveIntervention(context.Context, taskstore.BackgroundRun, taskstore.BackgroundRunOwnership) (backgroundruncoord.InterventionStatus, error)
	Wake()
}

type Config struct {
	WorkspaceID   task.WorkspaceID
	Store         Store
	Controller    Controller
	Terminal      *runterminal.Bridge
	Generator     *task.Generator
	ActorResolver func(context.Context) (task.ActorSnapshot, error)
	Now           func() time.Time
}

type Handler struct{ config Config }

func New(config Config) (*Handler, error) {
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil || config.Store == nil || config.Controller == nil ||
		config.Terminal == nil || config.Generator == nil || config.ActorResolver == nil || config.Now == nil {
		return nil, errors.New("valid run control configuration is required")
	}
	return &Handler{config: config}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	actor, err := h.config.ActorResolver(r.Context())
	if err != nil || actor.Validate() != nil || (actor.Type != task.ActorDevice && actor.Type != task.ActorOperator) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.EscapedPath() != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == HTMLPathPrefix || r.URL.Path == HTMLPathPrefix+"/" {
		h.serveListPage(w, r, actor)
		return
	}
	if strings.HasPrefix(r.URL.Path, HTMLPathPrefix+"/") {
		id, parseErr := task.ParseTaskID(strings.TrimPrefix(r.URL.Path, HTMLPathPrefix+"/"))
		if parseErr != nil {
			http.NotFound(w, r)
			return
		}
		h.serveRunPage(w, r, actor, id)
		return
	}
	if r.URL.Path == APIPathPrefix {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			methodNotAllowed(w, "GET")
			return
		}
		h.list(w, r, actor)
		return
	}
	if !strings.HasPrefix(r.URL.Path, APIPathPrefix+"/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, APIPathPrefix+"/"), "/")
	id, parseErr := task.ParseTaskID(parts[0])
	if parseErr != nil || len(parts) > 3 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet || r.URL.RawQuery != "" {
			methodNotAllowed(w, "GET")
			return
		}
		h.get(w, r, actor, id)
		return
	}
	if len(parts) == 3 && parts[1] == "terminal" && (parts[2] == taskenvdocker.ShellRoleInspector || parts[2] == taskenvdocker.ShellRoleHuman) {
		h.terminal(w, r, actor, id, parts[2])
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost || r.URL.RawQuery != "" {
		methodNotAllowed(w, "POST")
		return
	}
	switch parts[1] {
	case "interrupt":
		h.interrupt(w, r, actor, id)
	case "steer":
		h.steer(w, r, actor, id)
	case "takeover":
		h.transfer(w, r, actor, id, false)
	case "handback":
		h.transfer(w, r, actor, id, true)
	default:
		http.NotFound(w, r)
	}
}

type projection struct {
	RunID            task.TaskID                           `json:"run_id"`
	RunState         taskstore.BackgroundRunState          `json:"run_state"`
	Ownership        taskstore.BackgroundRunOwnershipMode  `json:"ownership"`
	OwnershipPhase   taskstore.BackgroundRunOwnershipPhase `json:"ownership_phase"`
	WriterGeneration int64                                 `json:"writer_generation"`
	Intervention     string                                `json:"intervention"`
	Questions        int                                   `json:"questions"`
	Permissions      int                                   `json:"permissions"`
	LastControl      string                                `json:"last_control,omitempty"`
	LastControlState taskstore.BackgroundRunControlState   `json:"last_control_state,omitempty"`
}

func (h *Handler) project(ctx context.Context, run taskstore.BackgroundRun, ownership taskstore.BackgroundRunOwnership) projection {
	result := projection{RunID: run.TaskID, RunState: run.State, Ownership: ownership.Mode, OwnershipPhase: ownership.Phase,
		WriterGeneration: ownership.WriterGeneration, Intervention: "unavailable"}
	if ownership.Mode == taskstore.BackgroundRunAgentOwned && run.EffectPhase == taskstore.BackgroundRunEffectPromptAdmitted && run.CancelEpoch == 0 {
		status, err := h.config.Controller.ObserveIntervention(ctx, run, ownership)
		if err != nil {
			result.Intervention = "uncertain"
		} else {
			result.Intervention, result.Questions, result.Permissions = status.State, status.Questions, status.Permissions
		}
	} else if ownership.Mode == taskstore.BackgroundRunHumanOwned {
		result.Intervention = "human_terminal"
	} else if ownership.Mode == taskstore.BackgroundRunTakeoverRequested || ownership.Mode == taskstore.BackgroundRunHandbackRequested {
		result.Intervention = "routes_closed"
	}
	if control, err := h.config.Store.LatestBackgroundRunControl(ctx, run.WorkspaceID, run.TaskID); err == nil {
		result.LastControl, result.LastControlState = control.CommandKind, control.State
	}
	return result
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	values, err := h.config.Store.ListBackgroundRunsForControl(r.Context(), h.config.WorkspaceID, actor, 100)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	runs := make([]projection, 0, len(values))
	for _, value := range values {
		runs = append(runs, projection{RunID: value.Run.TaskID, RunState: value.Run.State, Ownership: value.Ownership.Mode,
			OwnershipPhase: value.Ownership.Phase, WriterGeneration: value.Ownership.WriterGeneration, Intervention: "not_polled"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	run, ownership, err := h.config.Store.GetBackgroundRunForControl(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.project(r.Context(), run, ownership))
}

func (h *Handler) interrupt(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	if !emptyJSON(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	admission, err := h.admitControl(r.Context(), actor, id, key, taskstore.InterruptBackgroundRunCommand, "")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		h.config.Controller.Wake()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "control_id": admission.Control.ReceiptID, "intervention": admission.Control.State})
}

func (h *Handler) steer(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		Instruction string `json:"instruction"`
	}
	if !strictJSON(w, r, &input) {
		return
	}
	if !validInstruction(input.Instruction) {
		http.Error(w, "invalid instruction", http.StatusBadRequest)
		return
	}
	admission, err := h.admitControl(r.Context(), actor, id, key, taskstore.SteerBackgroundRunCommand, input.Instruction)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		h.config.Controller.Wake()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "control_id": admission.Control.ReceiptID, "intervention": admission.Control.State})
}

func (h *Handler) admitControl(ctx context.Context, actor task.ActorSnapshot, id task.TaskID, key task.IdempotencyKey, command, instruction string) (taskstore.BackgroundRunControlAdmission, error) {
	receiptID, err := h.config.Generator.ReceiptID()
	if err != nil {
		return taskstore.BackgroundRunControlAdmission{}, err
	}
	var messageID task.OpenCodeMessageID
	if command == taskstore.SteerBackgroundRunCommand {
		messageID, err = h.config.Generator.OpenCodeMessageID()
		if err != nil {
			return taskstore.BackgroundRunControlAdmission{}, err
		}
	}
	requestHash := commandHash(command, struct {
		RunID       task.TaskID `json:"run_id"`
		Instruction string      `json:"instruction,omitempty"`
	}{id, instruction})
	return h.config.Store.AdmitBackgroundRunControl(ctx, taskstore.AdmitBackgroundRunControlParams{
		WorkspaceID: h.config.WorkspaceID, TaskID: id, ReceiptID: receiptID, OpenCodeMessageID: messageID, Instruction: instruction,
		Claim:              task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: command}, Key: key, RequestHash: requestHash, Actor: actor},
		APIContractVersion: APIContractVersion, RequestedAt: h.config.Now().UTC().Truncate(time.Millisecond),
	})
}

func (h *Handler) transfer(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID, handback bool) {
	if !emptyJSON(w, r) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	command := taskstore.RequestBackgroundRunTakeoverCommand
	if handback {
		command = taskstore.RequestBackgroundRunHandbackCommand
	}
	requestHash := commandHash(command, struct {
		RunID task.TaskID `json:"run_id"`
	}{id})
	receiptID, err := h.config.Generator.ReceiptID()
	if err != nil {
		http.Error(w, "identity generation failed", http.StatusInternalServerError)
		return
	}
	claim := task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: h.config.WorkspaceID, CommandKind: command}, Key: key, RequestHash: requestHash, Actor: actor}
	now := h.config.Now().UTC().Truncate(time.Millisecond)
	var admission taskstore.BackgroundRunOwnershipAdmission
	if handback {
		admission, err = h.config.Store.RequestBackgroundRunHandback(r.Context(), taskstore.RequestBackgroundRunHandbackParams{
			WorkspaceID: h.config.WorkspaceID, TaskID: id, ReceiptID: receiptID, Claim: claim, APIContractVersion: APIContractVersion, RequestedAt: now})
	} else {
		admission, err = h.config.Store.RequestBackgroundRunTakeover(r.Context(), taskstore.RequestBackgroundRunTakeoverParams{
			WorkspaceID: h.config.WorkspaceID, TaskID: id, ReceiptID: receiptID, Claim: claim, APIContractVersion: APIContractVersion, RequestedAt: now})
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if admission.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	} else {
		h.config.Controller.Wake()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "ownership": admission.Ownership.Mode, "committed": true})
}

func (h *Handler) terminal(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID, role string) {
	run, ownership, err := h.config.Store.GetBackgroundRunForControl(r.Context(), h.config.WorkspaceID, id, actor)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h.config.Terminal.Serve(w, r, run, ownership, role)
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (task.IdempotencyKey, bool) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		http.Error(w, "valid Idempotency-Key required", http.StatusBadRequest)
		return "", false
	}
	key, err := task.ParseIdempotencyKey(values[0])
	if err != nil {
		http.Error(w, "valid Idempotency-Key required", http.StatusBadRequest)
		return "", false
	}
	return key, true
}

func strictJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Header.Get("Content-Type") != "application/json" || r.URL.RawQuery != "" {
		http.Error(w, "application/json without query parameters required", http.StatusBadRequest)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil || jsoncanon.Check(payload, 3) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func emptyJSON(w http.ResponseWriter, r *http.Request) bool {
	var input struct{}
	return strictJSON(w, r, &input)
}

func validInstruction(value string) bool {
	if value == "" || len(value) > 16*1024 || utf8.RuneCountInString(value) > maxInstruction || strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return false
		}
	}
	return true
}

func commandHash(command string, value any) task.RequestHash {
	wire, _ := json.Marshal(value)
	return task.RequestHash(sha256.Sum256(append(append([]byte(command), '\n'), wire...)))
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, taskstore.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, taskstore.ErrInvalidState), errors.Is(err, backgroundruncoord.ErrInterventionUnavailable):
		http.Error(w, "run is not eligible for this operation", http.StatusConflict)
	case errors.Is(err, taskstore.ErrIdempotencyConflict):
		http.Error(w, "idempotency conflict", http.StatusConflict)
	default:
		http.Error(w, "run control unavailable", http.StatusServiceUnavailable)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

type pageView struct {
	Nonce string
	RunID task.TaskID
	Runs  []taskstore.BackgroundRunControlView
}

func (h *Handler) serveListPage(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	runs, err := h.config.Store.ListBackgroundRunsForControl(r.Context(), h.config.WorkspaceID, actor, 100)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	nonce, err := pageNonce()
	if err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)
		return
	}
	setPageHeaders(w, nonce)
	if r.Method == http.MethodGet {
		_ = listTemplate.Execute(w, pageView{Nonce: nonce, Runs: runs})
	}
}

func (h *Handler) serveRunPage(w http.ResponseWriter, r *http.Request, actor task.ActorSnapshot, id task.TaskID) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	if _, _, err := h.config.Store.GetBackgroundRunForControl(r.Context(), h.config.WorkspaceID, id, actor); err != nil {
		writeStoreError(w, err)
		return
	}
	nonce, err := pageNonce()
	if err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)
		return
	}
	setPageHeaders(w, nonce)
	if r.Method == http.MethodGet {
		_ = runTemplate.Execute(w, pageView{Nonce: nonce, RunID: id})
	}
}

func pageNonce() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func setPageHeaders(w http.ResponseWriter, nonce string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; connect-src 'self' ws: wss:; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "same-origin")
}

var listTemplate = template.Must(template.New("runs").Parse(listPage))
var runTemplate = template.Must(template.New("run").Parse(runPage))

const listPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Fern Runs</title><style>` + pageCSS + `</style></head><body><main><header><a href="/fern/">FERN / CONTROL DECK</a><h1>Active field notes</h1><p>Every row names a durable run and its current writer generation.</p></header><section class="ledger">{{range .Runs}}<a class="row" href="/fern/runs/{{.Run.TaskID}}"><span><b>{{.Run.TaskID}}</b><small>{{.Run.State}}</small></span><span class="stamp">{{.Ownership.Mode}} · W{{.Ownership.WriterGeneration}}</span></a>{{else}}<p class="empty">No background runs recorded.</p>{{end}}</section></main></body></html>`

const runPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Fern Run</title><style>` + pageCSS + `</style></head><body><main data-run="{{.RunID}}"><header><a href="/fern/runs">FERN / RUNS</a><h1>Writer control deck</h1><p class="mono">{{.RunID}}</p></header><section class="status-grid"><article><small>RUN</small><strong id="run-state">loading</strong></article><article><small>OWNER</small><strong id="owner">loading</strong></article><article><small>LOCAL SIGNAL</small><strong id="signal">loading</strong></article><article><small>GENERATION</small><strong id="generation">-</strong></article></section><section class="controls"><button id="interrupt">Interrupt locally</button><button id="inspect">Read-only shell</button><textarea id="instruction" maxlength="16384" placeholder="Steer the agent with a precise next instruction"></textarea><button id="steer">Send and resume</button><button class="danger" id="takeover">Cold writable takeover</button><button id="human" hidden>Open human shell</button><button class="danger" id="handback" hidden>Stop human and hand back</button><p id="notice" role="status">Loading exact ownership…</p></section><section id="terminal-wrap" hidden><div class="terminal-bar"><span id="terminal-kind">TERMINAL</span><button id="close-terminal">Close</button></div><pre id="terminal" tabindex="0"></pre><small>Workspace access is enforced by the container mount. Inspector mode may observe files while the agent changes them.</small></section></main><script nonce="{{.Nonce}}">` + runScript + `</script></body></html>`

const pageCSS = `:root{color-scheme:dark;--ink:#ecf0df;--muted:#94a38b;--line:#45533d;--acid:#d8ff72;--rust:#ff8b66;--ground:#10150e;--panel:#171e14;font-family:"Avenir Next Condensed","Arial Narrow",sans-serif}*{box-sizing:border-box}body{margin:0;min-height:100dvh;color:var(--ink);background:repeating-linear-gradient(90deg,#ffffff05 0 1px,transparent 1px 48px),radial-gradient(circle at 80% -10%,#415529 0,transparent 36%),var(--ground)}main{width:min(100% - 28px,920px);margin:auto;padding:42px 0 80px}header{border-left:8px solid var(--acid);padding:4px 0 8px 20px;margin-bottom:32px}header a{color:var(--acid);font:700 12px ui-monospace,monospace;letter-spacing:.18em;text-decoration:none}h1{margin:12px 0 4px;font-size:clamp(36px,8vw,72px);line-height:.88;letter-spacing:-.055em;text-transform:uppercase}p{color:var(--muted)}.mono,.stamp,small{font-family:ui-monospace,SFMono-Regular,monospace}.ledger,.controls,.status-grid,#terminal-wrap{border:1px solid var(--line);background:#171e14e8}.row{display:flex;justify-content:space-between;gap:18px;padding:20px;color:var(--ink);text-decoration:none;border-top:1px solid var(--line)}.row:first-child{border-top:0}.row:hover{background:#d8ff720c}.row small{display:block;margin-top:6px;color:var(--muted)}.stamp{align-self:center;color:var(--acid);font-size:12px}.empty{padding:20px}.status-grid{display:grid;grid-template-columns:repeat(4,1fr);margin-bottom:14px}.status-grid article{padding:17px;border-left:1px solid var(--line)}.status-grid article:first-child{border-left:0}.status-grid small{display:block;color:var(--muted);font-size:10px;letter-spacing:.14em}.status-grid strong{display:block;margin-top:8px;text-transform:uppercase}.controls{display:grid;grid-template-columns:1fr 1fr;gap:10px;padding:18px}.controls textarea,.controls p{grid-column:1/-1}button,textarea{border:1px solid #65735d;background:#20291c;color:var(--ink);font:600 15px inherit}button{padding:13px 16px;cursor:pointer;text-transform:uppercase;letter-spacing:.04em}button:hover{border-color:var(--acid);color:var(--acid)}button:disabled{opacity:.35;cursor:not-allowed}.danger{border-color:#80503f;color:#ffb299}textarea{min-height:108px;padding:14px;resize:vertical}#notice{margin:4px 0 0;min-height:24px}#terminal-wrap{margin-top:14px}.terminal-bar{display:flex;justify-content:space-between;align-items:center;padding:8px 12px;border-bottom:1px solid var(--line);font:700 11px ui-monospace,monospace;letter-spacing:.15em;color:var(--acid)}.terminal-bar button{padding:5px 9px;font-size:11px}#terminal{height:420px;margin:0;padding:16px;overflow:auto;background:#090c08;color:#d8ff72;font:13px/1.45 ui-monospace,SFMono-Regular,monospace;white-space:pre-wrap;outline:none}@media(max-width:650px){main{padding-top:24px}.status-grid{grid-template-columns:1fr 1fr}.status-grid article:nth-child(3){border-left:0;border-top:1px solid var(--line)}.status-grid article:nth-child(4){border-top:1px solid var(--line)}.controls{grid-template-columns:1fr}#terminal{height:55dvh}}`

const runScript = `const root=document.querySelector('main'),id=root.dataset.run,api='/fern/api/v1/runs/'+encodeURIComponent(id),notice=document.querySelector('#notice'),encoder=new TextEncoder(),decoder=new TextDecoder();let socket,loading=false;async function csrf(method,path){const r=await fetch('/fern/api/v1/csrf?method='+method+'&path='+encodeURIComponent(path));if(!r.ok)return '';return (await r.json()).token||''}async function mutate(action,body={}){const path=api+'/'+action,key=crypto.randomUUID(),token=await csrf('POST',path);notice.textContent='Committing '+action+' intent…';const headers={'Content-Type':'application/json','Idempotency-Key':key};if(token)headers['X-Fern-CSRF-Token']=token;const r=await fetch(path,{method:'POST',headers,body:JSON.stringify(body)});notice.textContent=r.ok?'Durable intent accepted. Waiting for exact evidence…':await r.text();await load()}async function load(){if(loading)return;loading=true;try{const r=await fetch(api);if(!r.ok){notice.textContent=await r.text();return}const v=await r.json();document.querySelector('#run-state').textContent=v.run_state;document.querySelector('#owner').textContent=v.ownership;document.querySelector('#signal').textContent=v.intervention;document.querySelector('#generation').textContent='W'+v.writer_generation;const agent=v.ownership==='agent_owned',human=v.ownership==='human_owned',busy=v.last_control_state==='requested'||v.last_control_state==='attempted';document.querySelector('#interrupt').disabled=!agent||busy;document.querySelector('#inspect').disabled=!agent;document.querySelector('#steer').disabled=!agent||busy;document.querySelector('#takeover').disabled=!agent||busy;document.querySelector('#human').hidden=!human;document.querySelector('#handback').hidden=!human;if(v.last_control_state==='uncertain')notice.textContent=v.last_control+' has an uncertain outcome; it will not be blindly replayed.';else if(v.last_control_state==='conflict')notice.textContent=v.last_control+' was fenced by conflicting durable or upstream state.';else if(busy)notice.textContent=v.last_control+' is '+v.last_control_state+'.';else notice.textContent=v.intervention==='local_idle'?'OpenCode is idle in its current server process. This is not a global writer fence.':v.intervention==='routes_closed'?'Transfer in progress. Agent and human routes are closed.':'Exact ownership '+v.ownership+' at writer generation W'+v.writer_generation}finally{loading=false}}document.querySelector('#interrupt').onclick=()=>mutate('interrupt');document.querySelector('#steer').onclick=()=>mutate('steer',{instruction:document.querySelector('#instruction').value});document.querySelector('#takeover').onclick=()=>confirm('This permanently destroys the current OpenCode process and its state volume before enabling human writes. Continue?')&&mutate('takeover');document.querySelector('#handback').onclick=()=>confirm('Stop the human writer and start a fresh agent process and session?')&&mutate('handback');function terminal(role){if(socket)socket.close();const scheme=location.protocol==='https:'?'wss:':'ws:';socket=new WebSocket(scheme+'//'+location.host+api+'/terminal/'+role);socket.binaryType='arraybuffer';const panel=document.querySelector('#terminal-wrap'),out=document.querySelector('#terminal');panel.hidden=false;out.textContent='Connecting to '+role+' shell…\r\n';document.querySelector('#terminal-kind').textContent=role.toUpperCase()+' / WORKSPACE';socket.onmessage=e=>{const value=typeof e.data==='string'?e.data:decoder.decode(e.data,{stream:true});out.textContent=(out.textContent+value).slice(-524288);out.scrollTop=out.scrollHeight};socket.onclose=()=>{out.textContent=(out.textContent+'\r\n[terminal closed]\r\n').slice(-524288);socket=undefined};const send=value=>{if(socket&&socket.readyState===1)socket.send(encoder.encode(value))};out.focus();out.onkeydown=e=>{let value=e.ctrlKey&&e.key.toLowerCase()==='c'?'\x03':e.ctrlKey&&e.key.toLowerCase()==='d'?'\x04':e.key.length===1?e.key:{Enter:'\r',Backspace:'\x7f',Tab:'\t',ArrowUp:'\x1b[A',ArrowDown:'\x1b[B',ArrowRight:'\x1b[C',ArrowLeft:'\x1b[D'}[e.key];if(value){send(value);e.preventDefault()}};out.onpaste=e=>{send(e.clipboardData.getData('text'));e.preventDefault()}}document.querySelector('#inspect').onclick=()=>terminal('inspector');document.querySelector('#human').onclick=()=>terminal('human');document.querySelector('#close-terminal').onclick=()=>{if(socket)socket.close();document.querySelector('#terminal-wrap').hidden=true};load();setInterval(load,3000);`

var _ = fmt.Sprintf
