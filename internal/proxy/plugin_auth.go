package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/auth"
	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/safeio"
)

const (
	pluginAuthorizePath = "/fern/plugin-auth/authorize"
	maxPluginAuthBody   = 4 << 10
	pluginBearerRealm   = "fern-plugin"
	pluginClientName    = "OpenCode plugin"
)

var pluginAuthorizationTemplate = newPage("plugin-authorization", "Authorize OpenCode", dialogCSS+`
h1{margin:24px 0 8px;font-size:32px}p,li{color:#bdcbb5;line-height:1.5}.code{margin:20px 0;padding:14px;border:1px solid #52664a;border-radius:13px;background:#11180f;text-align:center;font:700 20px ui-monospace,monospace;letter-spacing:.08em}.actions{display:grid;grid-template-columns:1fr 1fr;gap:10px}button{padding:14px;border:0;border-radius:14px;font:inherit;font-weight:750;cursor:pointer}button.approve{background:#b9ef86;color:#15200f}button.deny{background:#472a26;color:#ffcbc2}button:disabled{opacity:.55}#status{min-height:24px;margin-top:16px}`,
	`<main id="authorization" data-code="{{.Code}}" data-approve="{{.ApprovePath}}" data-deny="{{.DenyPath}}"><div class="mark">F</div><h1>Authorize {{.Client}}?</h1><p>This grants one OpenCode plugin access to Fern Background Runs with these fixed permissions:</p><ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul><div class="code">{{.Code}}</div><div class="actions"><button class="deny" data-decision="deny">Deny</button><button class="approve" data-decision="approve">Authorize</button></div><p id="status" role="status"></p></main>
<script nonce="{{.Nonce}}">const root=document.getElementById('authorization'),status=document.getElementById('status'),buttons=[...document.querySelectorAll('button[data-decision]')];async function decide(decision){buttons.forEach(button=>button.disabled=true);const path=root.dataset[decision];try{const query=new URLSearchParams({method:'POST',path}),csrfResponse=await fetch('/fern/api/v1/csrf?'+query.toString(),{credentials:'same-origin'}),csrf=await csrfResponse.json();if(!csrfResponse.ok||!csrf.token)throw new Error('Could not prepare authorization');const response=await fetch(path,{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','X-Fern-CSRF-Token':csrf.token},body:JSON.stringify({user_code:root.dataset.code})});if(!response.ok)throw new Error('Authorization request failed');status.textContent=decision==='approve'?'OpenCode is authorized. You may close this page.':'Authorization denied. You may close this page.'}catch(error){status.textContent=error.message;buttons.forEach(button=>button.disabled=false)}}buttons.forEach(button=>button.addEventListener('click',()=>decide(button.dataset.decision)));</script>`)

type pluginAuthorizationPage struct {
	Client, Code, ApprovePath, DenyPath, Nonce string
	Scopes                                     []string
}

type pluginAuthHTTP struct {
	store *auth.PluginStore
	now   func() time.Time
}

func newPluginAuthHTTP(store *auth.PluginStore) *pluginAuthHTTP {
	return &pluginAuthHTTP{store: store, now: time.Now}
}

func (handler *pluginAuthHTTP) start(writer http.ResponseWriter, request *http.Request) {
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
		writePluginAuthError(writer, err, 0)
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
		int64(result.ExpiresAt.Sub(now).Seconds()), int64(result.Interval.Seconds()), auth.Scopes()}, nil)
}

func (handler *pluginAuthHTTP) poll(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodePluginAuthJSON(writer, request, &body) {
		return
	}
	now := handler.now()
	result, err := handler.store.Poll(body.DeviceCode, now)
	if err != nil {
		writePluginAuthError(writer, err, pollIntervalSeconds)
		return
	}
	switch result.State {
	case auth.PollPending:
		writeJSONStatus(writer, http.StatusAccepted, map[string]string{"status": "pending"}, nil)
	case auth.PollDenied:
		writeJSONStatus(writer, http.StatusForbidden, map[string]string{"status": "denied"}, nil)
	case auth.PollExpired:
		writeJSONStatus(writer, http.StatusGone, map[string]string{"status": "expired"}, nil)
	case auth.PollApproved:
		writeJSONStatus(writer, http.StatusOK, struct {
			AccessToken  string   `json:"access_token"`
			TokenType    string   `json:"token_type"`
			CredentialID string   `json:"credential_id"`
			ExpiresIn    int64    `json:"expires_in"`
			Scopes       []string `json:"scopes"`
		}{body.DeviceCode, "Bearer", result.CredentialID, int64(result.ExpiresAt.Sub(now).Seconds()), auth.Scopes()}, nil)
	default:
		writeUnavailable(writer, "plugin authorization")
	}
}

// authenticate is the plugin realm: an exact fixed-scope bearer whose request
// is registered against the credential so revocation cancels it. Scope checks
// stay with the run APIs.
func (handler *pluginAuthHTTP) authenticate(writer http.ResponseWriter, request *http.Request) (*http.Request, func(), bool) {
	token, ok := exactBearer(request.Header.Values("Authorization"))
	if !ok {
		rejectPluginBearer(writer)
		return nil, nil, false
	}
	now := handler.now()
	credential, valid, err := handler.store.Authenticate(token, now)
	if err != nil {
		writeUnavailable(writer, "plugin authorization")
		return nil, nil, false
	}
	if !valid {
		rejectPluginBearer(writer)
		return nil, nil, false
	}
	ctx, cancel := context.WithDeadline(request.Context(), credential.ExpiresAt)
	unregister, admitted := handler.store.RegisterRequest(credential.ID, now, cancel)
	if !admitted {
		cancel()
		rejectPluginBearer(writer)
		return nil, nil, false
	}
	release := func() {
		unregister()
		cancel()
	}
	actor := domain.ActorSnapshot{
		Type: domain.ActorOpenCode, ID: credential.ID, DisplayName: pluginClientName,
		CredentialID: credential.ID, Authentication: "fern_plugin_bearer", RequestID: rand.Text(),
	}
	ctx = auth.WithRequestAuthorization(ctx, credential)
	request = request.WithContext(domain.WithActor(ctx, actor))
	stripCredentials(request)
	return request, release, true
}

func (handler *pluginAuthHTTP) revokeSelf(writer http.ResponseWriter, request *http.Request) {
	if request.Body != nil && request.ContentLength != 0 {
		var body struct{}
		if !decodePluginAuthJSON(writer, request, &body) {
			return
		}
	}
	authorization, _ := auth.RequestAuthorizationFromContext(request.Context())
	actor, err := domain.ContextActor(request.Context())
	if err == nil {
		err = handler.store.Revoke(authorization.Credential.ID, actor, handler.now())
	}
	if err != nil {
		writePluginAuthError(writer, err, 0)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// authorizationPage lets a paired device approve or deny a pending plugin
// authorization; the page's script fetches a CSRF token for its decision.
func (handler *pluginAuthHTTP) authorizationPage(writer http.ResponseWriter, request *http.Request) {
	values, err := url.ParseQuery(request.URL.RawQuery)
	id, code := values.Get("id"), values.Get("code")
	if err != nil || !exactQuery(values, "id", "code") {
		http.Error(writer, "invalid plugin authorization link", http.StatusBadRequest)
		return
	}
	if !handler.store.Pending(id, code, handler.now()) {
		http.NotFound(writer, request)
		return
	}
	nonce := rand.Text()
	approvePath := "/fern/api/plugin-auth/requests/" + id + "/approve"
	denyPath := "/fern/api/plugin-auth/requests/" + id + "/deny"
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pluginAuthorizationTemplate.Execute(writer, pluginAuthorizationPage{pluginClientName, code, approvePath, denyPath, nonce, auth.Scopes()}); err != nil {
		http.Error(writer, "render plugin authorization", http.StatusInternalServerError)
	}
}

func (handler *pluginAuthHTTP) decide(approve bool) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		actor, err := domain.ContextActor(request.Context())
		if err != nil {
			http.Error(writer, "trusted actor unavailable", http.StatusUnauthorized)
			return
		}
		var body struct {
			UserCode string `json:"user_code"`
		}
		if !decodePluginAuthJSON(writer, request, &body) {
			return
		}
		id := request.PathValue("id")
		if approve {
			_, err = handler.store.Approve(request.Context(), id, body.UserCode, actor, handler.now())
		} else {
			err = handler.store.Deny(request.Context(), id, body.UserCode, actor, handler.now())
		}
		if err != nil {
			writePluginAuthError(writer, err, 0)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}
}

func (handler *pluginAuthHTTP) credentials(writer http.ResponseWriter, _ *http.Request) {
	credentials, err := handler.store.Credentials(handler.now())
	writeJSON(writer, struct {
		Credentials []auth.Credential `json:"credentials"`
		Scopes      []string          `json:"scopes"`
	}{credentials, auth.Scopes()}, err)
}

func (handler *pluginAuthHTTP) revokeCredential(writer http.ResponseWriter, request *http.Request) {
	actor, err := domain.ContextActor(request.Context())
	if err == nil {
		err = handler.store.Revoke(request.PathValue("id"), actor, handler.now())
	}
	switch {
	case errors.Is(err, auth.ErrNotFound) || errors.Is(err, os.ErrNotExist):
		http.NotFound(writer, request)
	case err != nil:
		writePluginAuthError(writer, err, 0)
	default:
		writer.WriteHeader(http.StatusNoContent)
	}
}

func decodePluginAuthJSON(writer http.ResponseWriter, request *http.Request, value any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(writer, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxPluginAuthBody)
	payload, err := io.ReadAll(request.Body)
	if err != nil || safeio.CheckJSON(payload, 3) != nil {
		http.Error(writer, "invalid plugin authorization request", http.StatusBadRequest)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
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

// bearerLike reports whether one Authorization value uses the Bearer scheme,
// in any case, so it can never fall through to another realm.
func bearerLike(value string) bool {
	if len(value) < len("Bearer") || !strings.EqualFold(value[:len("Bearer")], "Bearer") {
		return false
	}
	return len(value) == len("Bearer") || value[len("Bearer")] == ' ' || value[len("Bearer")] == '\t'
}

func pluginVerificationURI(request *http.Request) (string, bool) {
	origin, ok := request.Context().Value(originKey{}).(trustedOrigin)
	if !ok || origin.raw == "" {
		return "", false
	}
	return origin.raw + pluginAuthorizePath, true
}

func rejectPluginBearer(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Bearer realm="`+pluginBearerRealm+`"`)
	http.Error(writer, "unauthorized", http.StatusUnauthorized)
}

// pollIntervalSeconds is the retry interval every poll exposes, valid or not,
// so it never reveals whether a code exists.
const pollIntervalSeconds = 5

// writePluginAuthError maps an auth plugin error to its HTTP status. A rate
// limit carries retryAfter seconds, at least one.
func writePluginAuthError(writer http.ResponseWriter, err error, retryAfter int) {
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		if retryAfter <= 0 {
			retryAfter = 1
		}
		writer.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		http.Error(writer, "plugin authorization temporarily limited", http.StatusTooManyRequests)
	case errors.Is(err, auth.ErrCapacity):
		http.Error(writer, "plugin authorization capacity reached", http.StatusTooManyRequests)
	case errors.Is(err, auth.ErrInvalidCode), errors.Is(err, auth.ErrNotFound):
		http.Error(writer, "plugin authorization not found", http.StatusUnauthorized)
	case errors.Is(err, auth.ErrInvalidState):
		http.Error(writer, "plugin authorization is no longer pending", http.StatusConflict)
	default:
		writeUnavailable(writer, "plugin authorization")
	}
}
