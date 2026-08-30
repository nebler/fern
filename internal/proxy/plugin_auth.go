package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/jsoncanon"
	"github.com/nebler/fern/internal/pluginauth"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskapi"
)

const (
	pluginAuthStartPath = "/fern/api/plugin-auth/start"
	pluginAuthPollPath  = "/fern/api/plugin-auth/poll"
	pluginAuthSelfPath  = "/fern/api/plugin-auth/self/revoke"
	pluginAuthorizePath = "/fern/plugin-auth/authorize"
	maxPluginAuthBody   = 4 << 10
	pluginBearerRealm   = "fern-plugin"
	pluginClientName    = "OpenCode plugin"
)

var pluginAuthorizationTemplate = template.Must(template.New("plugin-authorization").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize OpenCode</title>
<style>:root{font-family:ui-rounded,"SF Pro Rounded","Avenir Next",system-ui,sans-serif;color:#f3f7e9;background:#11180f}*{box-sizing:border-box}body{display:grid;min-height:100dvh;margin:0;padding:24px 18px;background:radial-gradient(circle at 15% 0,#314829 0,transparent 42%),#11180f}main{width:min(100%,520px);margin:auto;padding:28px;border:1px solid #52664a;border-radius:26px;background:#182116}.mark{display:grid;place-items:center;width:54px;height:54px;border-radius:18px;background:#b9ef86;color:#162210;font-size:28px;font-weight:800}h1{margin:24px 0 8px;font-size:32px}p,li{color:#bdcbb5;line-height:1.5}.code{margin:20px 0;padding:14px;border:1px solid #52664a;border-radius:13px;background:#11180f;text-align:center;font:700 20px ui-monospace,monospace;letter-spacing:.08em}.actions{display:grid;grid-template-columns:1fr 1fr;gap:10px}button{padding:14px;border:0;border-radius:14px;font:inherit;font-weight:750;cursor:pointer}button.approve{background:#b9ef86;color:#15200f}button.deny{background:#472a26;color:#ffcbc2}button:disabled{opacity:.55}#status{min-height:24px;margin-top:16px}</style></head>
<body><main id="authorization" data-code="{{.Code}}" data-approve="{{.ApprovePath}}" data-deny="{{.DenyPath}}"><div class="mark">F</div><h1>Authorize {{.Client}}?</h1><p>This grants one OpenCode plugin access to Fern Background Runs with these fixed permissions:</p><ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul><div class="code">{{.Code}}</div><div class="actions"><button class="deny" data-decision="deny">Deny</button><button class="approve" data-decision="approve">Authorize</button></div><p id="status" role="status"></p></main>
<script nonce="{{.Nonce}}">const root=document.getElementById('authorization'),status=document.getElementById('status'),buttons=[...document.querySelectorAll('button[data-decision]')];async function decide(decision){buttons.forEach(button=>button.disabled=true);const path=root.dataset[decision];try{const query=new URLSearchParams({method:'POST',path}),csrfResponse=await fetch('/fern/api/v1/csrf?'+query.toString(),{credentials:'same-origin'}),csrf=await csrfResponse.json();if(!csrfResponse.ok||!csrf.token)throw new Error('Could not prepare authorization');const response=await fetch(path,{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','X-Fern-CSRF-Token':csrf.token},body:JSON.stringify({user_code:root.dataset.code})});if(!response.ok)throw new Error('Authorization request failed');status.textContent=decision==='approve'?'OpenCode is authorized. You may close this page.':'Authorization denied. You may close this page.'}catch(error){status.textContent=error.message;buttons.forEach(button=>button.disabled=false)}}buttons.forEach(button=>button.addEventListener('click',()=>decide(button.dataset.decision)));</script></body></html>`))

type pluginAuthorizationPage struct {
	Client, Code, ApprovePath, DenyPath, Nonce string
	Scopes                                     []string
}

type pluginAuthHTTP struct {
	store *pluginauth.Store
	now   func() time.Time
}

func newPluginAuthHTTP(store *pluginauth.Store) *pluginAuthHTTP {
	return &pluginAuthHTTP{store: store, now: time.Now}
}

// remoteHandler admits the only unauthenticated plugin routes and intercepts
// every bearer attempt before paired-device authentication or workspace wake.
func (handler *pluginAuthHTTP) remoteHandler(paired, authenticated http.Handler) http.Handler {
	if handler == nil || handler.store == nil {
		return paired
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		if request.URL.EscapedPath() == path && (path == pluginAuthStartPath || path == pluginAuthPollPath) {
			request.Header.Del("Authorization")
			stripAllCookies(request)
			handler.servePublic(writer, request)
			return
		}
		if request.URL.EscapedPath() == path && (path == "/fern/pair" || path == "/fern/github/app/callback") {
			paired.ServeHTTP(writer, request)
			return
		}
		authorization := request.Header.Values("Authorization")
		if len(authorization) > 1 {
			setFernHeaders(writer.Header())
			if containsBearerLikeAuthorization(authorization) {
				rejectPluginBearer(writer)
			} else {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
			}
			return
		}
		if bearerLikeAuthorization(authorization) {
			handler.serveBearer(writer, request, authenticated)
			return
		}
		paired.ServeHTTP(writer, request)
	})
}

// rejectBearerHandler makes the loopback operator surface explicitly reject
// plugin bearer authority rather than allowing it to be interpreted as Basic.
func (handler *pluginAuthHTTP) rejectBearerHandler(next http.Handler) http.Handler {
	if handler == nil || handler.store == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values := request.Header.Values("Authorization")
		if len(values) > 1 || bearerLikeAuthorization(values) {
			http.NotFound(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (handler *pluginAuthHTTP) servePublic(writer http.ResponseWriter, request *http.Request) {
	setFernHeaders(writer.Header())
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, "POST")
		return
	}
	if request.URL.Path == pluginAuthStartPath {
		now := handler.now()
		verificationURI, ok := pluginVerificationURI(request)
		if !ok {
			writeUnavailable(writer, "trusted remote origin")
			return
		}
		var body struct{}
		if !decodePluginAuthJSON(writer, request, &body) {
			return
		}
		result, err := handler.store.Start(now)
		if err != nil {
			writePluginAuthError(writer, err)
			return
		}
		writeJSONStatus(writer, http.StatusCreated, struct {
			AuthorizationID         string   `json:"authorization_id"`
			DeviceCode              string   `json:"device_code"`
			UserCode                string   `json:"user_code"`
			VerificationURI         string   `json:"verification_uri"`
			VerificationURIComplete string   `json:"verification_uri_complete"`
			ExpiresIn               int64    `json:"expires_in"`
			Interval                int64    `json:"interval"`
			Scopes                  []string `json:"scopes"`
		}{result.AuthorizationID, result.DeviceCode, result.UserCode, verificationURI,
			verificationURI + "?" + url.Values{"id": {result.AuthorizationID}, "code": {result.UserCode}}.Encode(),
			int64(result.ExpiresAt.Sub(now).Seconds()), int64(result.Interval.Seconds()), fixedScopes()}, nil)
		return
	}
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodePluginAuthJSON(writer, request, &body) {
		return
	}
	now := handler.now()
	result, err := handler.store.Poll(body.DeviceCode, now)
	if err != nil {
		writePluginAuthErrorWithRetry(writer, err, int(pollIntervalSeconds()))
		return
	}
	switch result.State {
	case pluginauth.PollPending:
		writeJSONStatus(writer, http.StatusAccepted, map[string]string{"status": "pending"}, nil)
	case pluginauth.PollDenied:
		writeJSONStatus(writer, http.StatusForbidden, map[string]string{"status": "denied"}, nil)
	case pluginauth.PollExpired:
		writeJSONStatus(writer, http.StatusGone, map[string]string{"status": "expired"}, nil)
	case pluginauth.PollApproved:
		writeJSONStatus(writer, http.StatusOK, struct {
			AccessToken  string   `json:"access_token"`
			TokenType    string   `json:"token_type"`
			CredentialID string   `json:"credential_id"`
			ExpiresIn    int64    `json:"expires_in"`
			Scopes       []string `json:"scopes"`
		}{body.DeviceCode, "Bearer", result.CredentialID, int64(result.ExpiresAt.Sub(now).Seconds()), fixedScopes()}, nil)
	default:
		writeUnavailable(writer, "plugin authorization")
	}
}

func (handler *pluginAuthHTTP) serveBearer(writer http.ResponseWriter, request *http.Request, next http.Handler) {
	setFernHeaders(writer.Header())
	token, ok := exactBearer(request.Header.Values("Authorization"))
	if !ok {
		rejectPluginBearer(writer)
		return
	}
	now := handler.now()
	credential, valid, err := handler.store.Authenticate(token, now)
	if err != nil {
		writeUnavailable(writer, "plugin authorization")
		return
	}
	if !valid {
		rejectPluginBearer(writer)
		return
	}
	ctx, cancel := context.WithDeadline(request.Context(), credential.ExpiresAt)
	unregister, admitted := handler.store.RegisterRequest(credential.ID, now, cancel)
	if !admitted {
		cancel()
		rejectPluginBearer(writer)
		return
	}
	defer func() {
		unregister()
		cancel()
	}()
	requestID, err := randomCredential()
	if err != nil {
		http.Error(writer, "plugin identity unavailable", http.StatusInternalServerError)
		return
	}
	actor := task.ActorSnapshot{
		Type: task.ActorOpenCode, ID: credential.ID, DisplayName: pluginClientName,
		CredentialID: credential.ID, Authentication: "fern_plugin_bearer", RequestID: requestID,
	}
	ctx = pluginauth.WithRequestAuthorization(ctx, credential)
	ctx = taskapi.WithActor(ctx, actor)
	request = request.WithContext(ctx)
	request.Header.Del("Authorization")
	stripAllCookies(request)
	path := request.URL.Path
	if isMutation(request) && !sameOrigin(request) {
		http.Error(writer, "cross-origin plugin authorization request rejected", http.StatusForbidden)
		return
	}
	if request.URL.EscapedPath() != path {
		http.NotFound(writer, request)
		return
	}
	if path == pluginAuthSelfPath {
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, "POST")
			return
		}
		if request.Body != nil && request.ContentLength != 0 {
			var body struct{}
			if !decodePluginAuthJSON(writer, request, &body) {
				return
			}
		}
		unregister()
		if err := handler.store.Revoke(credential.ID, actor, now); err != nil {
			writePluginAuthError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if path == "/fern/api/runs" || strings.HasPrefix(path, "/fern/api/runs/") {
		next.ServeHTTP(writer, request)
		return
	}
	http.NotFound(writer, request)
}

// serveTrusted handles paired-device approval/denial and operator credential
// administration after the existing ingress authentication and CSRF checks.
func (handler *pluginAuthHTTP) serveTrusted(writer http.ResponseWriter, request *http.Request) bool {
	if handler == nil || handler.store == nil || request.URL.EscapedPath() != request.URL.Path {
		return false
	}
	path := request.URL.Path
	if path == pluginAuthorizePath {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, "GET")
			return true
		}
		actor, err := taskapi.ContextActor(request.Context())
		if err != nil || actor.Type != task.ActorDevice {
			http.NotFound(writer, request)
			return true
		}
		values, err := url.ParseQuery(request.URL.RawQuery)
		id, code := values.Get("id"), values.Get("code")
		if err != nil || len(values) != 2 || len(values["id"]) != 1 || len(values["code"]) != 1 || id == "" || code == "" {
			http.Error(writer, "invalid plugin authorization link", http.StatusBadRequest)
			return true
		}
		if !handler.store.Pending(id, code, handler.now()) {
			http.NotFound(writer, request)
			return true
		}
		nonce, err := randomCredential()
		if err != nil {
			http.Error(writer, "render plugin authorization", http.StatusInternalServerError)
			return true
		}
		approvePath := "/fern/api/plugin-auth/requests/" + id + "/approve"
		denyPath := "/fern/api/plugin-auth/requests/" + id + "/deny"
		setFernHeaders(writer.Header())
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pluginAuthorizationTemplate.Execute(writer, pluginAuthorizationPage{pluginClientName, code, approvePath, denyPath, nonce, fixedScopes()}); err != nil {
			http.Error(writer, "render plugin authorization", http.StatusInternalServerError)
		}
		return true
	}
	if isMutation(request) && !sameOrigin(request) {
		http.Error(writer, "cross-origin plugin authorization request rejected", http.StatusForbidden)
		return true
	}
	if strings.HasPrefix(path, "/fern/api/plugin-auth/requests/") {
		remainder := strings.TrimPrefix(path, "/fern/api/plugin-auth/requests/")
		parts := strings.Split(remainder, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] != "approve" && parts[1] != "deny" {
			http.NotFound(writer, request)
			return true
		}
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, "POST")
			return true
		}
		actor, err := taskapi.ContextActor(request.Context())
		if err != nil {
			http.Error(writer, "trusted actor unavailable", http.StatusUnauthorized)
			return true
		}
		var body struct {
			UserCode string `json:"user_code"`
		}
		if !decodePluginAuthJSON(writer, request, &body) {
			return true
		}
		if parts[1] == "approve" {
			_, err = handler.store.ApproveContext(request.Context(), parts[0], body.UserCode, actor, handler.now())
		} else {
			err = handler.store.DenyContext(request.Context(), parts[0], body.UserCode, actor, handler.now())
		}
		if err != nil {
			writePluginAuthError(writer, err)
			return true
		}
		writer.WriteHeader(http.StatusNoContent)
		return true
	}
	if path == "/fern/api/plugin-auth/credentials" {
		if request.Method != http.MethodGet {
			methodNotAllowed(writer, "GET")
			return true
		}
		credentials, err := handler.store.Credentials(handler.now())
		writeJSON(writer, struct {
			Credentials []pluginauth.Credential `json:"credentials"`
			Scopes      []string                `json:"scopes"`
		}{credentials, fixedScopes()}, err)
		return true
	}
	if strings.HasPrefix(path, "/fern/api/plugin-auth/credentials/") {
		id := strings.TrimPrefix(path, "/fern/api/plugin-auth/credentials/")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(writer, request)
			return true
		}
		if request.Method != http.MethodDelete {
			methodNotAllowed(writer, "DELETE")
			return true
		}
		actor, err := taskapi.ContextActor(request.Context())
		if err == nil {
			err = handler.store.Revoke(id, actor, handler.now())
		}
		if err != nil {
			if errors.Is(err, pluginauth.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
				http.NotFound(writer, request)
			} else {
				writePluginAuthError(writer, err)
			}
			return true
		}
		writer.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func decodePluginAuthJSON(writer http.ResponseWriter, request *http.Request, value any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(writer, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxPluginAuthBody)
	payload, err := io.ReadAll(request.Body)
	if err != nil || jsoncanon.Check(payload, 3) != nil {
		http.Error(writer, "invalid plugin authorization request", http.StatusBadRequest)
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(writer, "invalid plugin authorization request", http.StatusBadRequest)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(writer, "invalid plugin authorization request", http.StatusBadRequest)
		return false
	}
	return true
}

func exactBearer(values []string) (string, bool) {
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	return token, token != "" && !strings.ContainsAny(token, " \t\r\n,")
}

func bearerLikeAuthorization(values []string) bool {
	if len(values) != 1 {
		return false
	}
	value := values[0]
	if len(value) < len("Bearer") || !strings.EqualFold(value[:len("Bearer")], "Bearer") {
		return false
	}
	return len(value) == len("Bearer") || value[len("Bearer")] == ' ' || value[len("Bearer")] == '\t'
}

func containsBearerLikeAuthorization(values []string) bool {
	for _, value := range values {
		if bearerLikeAuthorization([]string{value}) {
			return true
		}
	}
	return false
}

func pluginVerificationURI(request *http.Request) (string, bool) {
	origin, ok := request.Context().Value(originKey{}).(trustedOrigin)
	if !ok || origin.legacy || origin.raw == "" {
		return "", false
	}
	return origin.raw + pluginAuthorizePath, true
}

func rejectPluginBearer(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="`+pluginBearerRealm+`"`)
	http.Error(writer, "unauthorized", http.StatusUnauthorized)
}

func pollIntervalSeconds() int64 {
	// Kept beside the HTTP vocabulary so all valid and invalid polls expose the
	// same retry interval without revealing whether a code exists.
	return 5
}

func fixedScopes() []string {
	return pluginauth.Scopes()
}

func writePluginAuthError(writer http.ResponseWriter, err error) {
	writePluginAuthErrorWithRetry(writer, err, 0)
}

func writePluginAuthErrorWithRetry(writer http.ResponseWriter, err error, retryAfter int) {
	switch {
	case errors.Is(err, pluginauth.ErrRateLimited):
		if retryAfter <= 0 {
			retryAfter = 1
		}
		writer.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		http.Error(writer, "plugin authorization temporarily limited", http.StatusTooManyRequests)
	case errors.Is(err, pluginauth.ErrCapacity):
		http.Error(writer, "plugin authorization capacity reached", http.StatusTooManyRequests)
	case errors.Is(err, pluginauth.ErrInvalidCode), errors.Is(err, pluginauth.ErrNotFound):
		http.Error(writer, "plugin authorization not found", http.StatusUnauthorized)
	case errors.Is(err, pluginauth.ErrInvalidState):
		http.Error(writer, "plugin authorization is no longer pending", http.StatusConflict)
	default:
		writeUnavailable(writer, "plugin authorization")
	}
}
