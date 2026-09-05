// Package backgroundroute owns the one fixed loopback listener used by the
// serial disposable Background Run lane.
package backgroundroute

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/taskstore"
)

var (
	ErrFenced   = errors.New("background route listener is fenced")
	ErrMismatch = errors.New("background route identity mismatch")
)

const (
	AttachmentUsername = "opencode"
	attachmentTTL      = 2 * time.Hour
	maxAttachments     = 16
)

// Identity is the complete immutable route-to-process binding.
type Identity struct {
	WorkspaceID      string
	TaskID           string
	AttemptID        string
	Generation       int64
	WriterGeneration int64
	SessionID        string
	RuntimeEpoch     int64
	ContainerID      string
	StartedAt        string
	RuntimeToken     string
}

// Target is constructed by the qualified Docker provider. Its authenticated
// transport is intentionally not inspectable by route or API callers.
type Target struct {
	endpoint  *url.URL
	transport http.RoundTripper
}

// NewTarget is the narrow provider-facing constructor for an authenticated
// exact-loopback proxy target.
func NewTarget(endpoint string, transport http.RoundTripper) (Target, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" || transport == nil {
		return Target{}, errors.New("exact authenticated loopback background target is required")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 || endpoint != "http://127.0.0.1:"+strconv.Itoa(port) {
		return Target{}, errors.New("canonical authenticated loopback background target is required")
	}
	copyURL := *parsed
	return Target{endpoint: &copyURL, transport: transport}, nil
}

type binding struct {
	identity Identity
	target   Target
	handler  http.Handler
	ctx      context.Context
	cancel   context.CancelFunc
	inflight int
	removed  bool
	drained  chan struct{}
}

type attachment struct {
	identity  Identity
	sessionID string
	expiresAt time.Time
}

// Attachment is a short-lived OpenCode client capability. The password is
// returned once and only its digest is retained by the route manager.
type Attachment struct {
	Origin    string
	Username  string
	Password  string
	ExpiresAt time.Time
}

// Manager owns one already-bound listener and never changes its public origin.
type Manager struct {
	mu          sync.Mutex
	listener    net.Listener
	server      *http.Server
	origin      *url.URL
	active      *binding
	pending     *binding
	attachments map[[sha256.Size]byte]attachment
	closing     bool
	run         bool
}

func New(listener net.Listener, origin string) (*Manager, error) {
	if listener == nil {
		return nil, errors.New("background route listener is required")
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() == "" || parsed.Port() == "443" {
		return nil, errors.New("exact private background route origin is required")
	}
	host, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, errors.New("background route listener must be exact loopback")
	}
	manager := &Manager{listener: listener, origin: parsed, attachments: make(map[[sha256.Size]byte]attachment)}
	manager.server = &http.Server{Handler: manager, ReadHeaderTimeout: 10 * time.Second}
	return manager, nil
}

// Run serves until cancellation, removing the route before HTTP shutdown.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	if m.run {
		m.mu.Unlock()
		return errors.New("background route server already started")
	}
	m.run = true
	m.mu.Unlock()
	served := make(chan error, 1)
	go func() { served <- m.server.Serve(m.listener) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		m.beginShutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := m.server.Shutdown(shutdownCtx)
		serveErr := <-served
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(err, serveErr)
	}
}

func (m *Manager) beginShutdown() {
	m.mu.Lock()
	m.closing = true
	clear(m.attachments)
	if m.active != nil {
		current := m.active
		m.active = nil
		current.removed = true
		current.cancel()
		if current.inflight == 0 {
			close(current.drained)
		}
	}
	m.mu.Unlock()
}

// Close makes the listener unreachable. It is safe after Run returned and is
// ordered before provider/Docker closure by composition.
func (m *Manager) Close() error {
	m.beginShutdown()
	return errors.Join(m.server.Close(), m.listener.Close())
}

// Activate installs one exact binding or proves the same binding is already
// installed. A removed binding fences all replacement until ConfirmRemoval.
func (m *Manager) Activate(identity Identity, target Target) (string, error) {
	if err := validateIdentity(identity); err != nil || target.endpoint == nil || target.transport == nil {
		return "", errors.Join(errors.New("valid exact background route activation is required"), err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return "", ErrFenced
	}
	if m.pending != nil {
		return "", ErrFenced
	}
	if m.active != nil {
		if m.active.identity != identity || m.active.target.endpoint.String() != target.endpoint.String() {
			return "", ErrFenced
		}
		return routeEvidence("active", identity), nil
	}
	current := &binding{identity: identity, target: target, drained: make(chan struct{})}
	current.ctx, current.cancel = context.WithCancel(context.Background())
	current.handler = m.reverseProxy(current)
	m.active = current
	return routeEvidence("active", identity), nil
}

// Remove unpublishes only the exact binding and waits for all requests admitted
// through it to stop using the target before returning evidence.
func (m *Manager) Remove(ctx context.Context, identity Identity) (string, error) {
	if err := validateIdentity(identity); err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.pending != nil {
		if m.pending.identity != identity {
			m.mu.Unlock()
			return "", ErrMismatch
		}
		drained := m.pending.drained
		m.mu.Unlock()
		select {
		case <-drained:
			return routeEvidence("removed", identity), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if m.active == nil {
		drained := make(chan struct{})
		close(drained)
		m.pending = &binding{identity: identity, removed: true, drained: drained}
		m.mu.Unlock()
		return routeEvidence("removed", identity), nil
	}
	if m.active.identity != identity {
		m.mu.Unlock()
		return "", ErrMismatch
	}
	current := m.active
	m.active = nil
	clear(m.attachments)
	m.pending = current
	current.removed = true
	current.cancel()
	if current.inflight == 0 {
		close(current.drained)
	}
	drained := current.drained
	m.mu.Unlock()
	select {
	case <-drained:
		return routeEvidence("removed", identity), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ConfirmRemoval clears the process-local reuse fence only after durable state
// positively reports route_removed for the same identity.
func (m *Manager) ConfirmRemoval(identity Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		if m.active == nil {
			return nil
		}
		return ErrMismatch
	}
	if m.pending.identity != identity {
		return ErrMismatch
	}
	select {
	case <-m.pending.drained:
		m.pending = nil
		return nil
	default:
		return ErrFenced
	}
}

func (m *Manager) Active(identity Identity) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closing && m.active != nil && m.active.identity == identity
}

func (m *Manager) Origin() string { return m.origin.String() }

// IssueAttachment creates an expiring capability only when the complete
// durable run tuple still names the active OpenCode runtime.
func (m *Manager) IssueAttachment(run taskstore.BackgroundRun) (Attachment, bool, error) {
	identity, ok := identityFromRun(run)
	if !ok || string(run.OpenCodeSessionID) != identity.SessionID {
		return Attachment{}, false, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Attachment{}, false, fmt.Errorf("create attachment capability: %w", err)
	}
	password := base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(password))
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing || m.active == nil || m.active.identity != identity {
		return Attachment{}, false, nil
	}
	for key, value := range m.attachments {
		if !value.expiresAt.After(now) {
			delete(m.attachments, key)
		}
	}
	if len(m.attachments) >= maxAttachments {
		return Attachment{}, false, errors.New("too many active run attachments")
	}
	expiresAt := now.Add(attachmentTTL)
	m.attachments[digest] = attachment{identity: identity, sessionID: string(run.OpenCodeSessionID), expiresAt: expiresAt}
	return Attachment{Origin: m.Origin(), Username: AttachmentUsername, Password: password, ExpiresAt: expiresAt}, true, nil
}

// ActiveOrigin returns the configured origin only when the complete durable run
// tuple still names the process currently bound to the listener.
func (m *Manager) ActiveOrigin(run taskstore.BackgroundRun) (string, bool) {
	identity, ok := identityFromRun(run)
	if !ok || !m.Active(identity) {
		return "", false
	}
	return m.Origin(), true
}

func identityFromRun(run taskstore.BackgroundRun) (Identity, bool) {
	started, err := time.Parse(time.RFC3339Nano, run.ObservedContainerStartedAt)
	if err != nil || started.UnixNano() != run.RuntimeEpoch {
		return Identity{}, false
	}
	digest := sha256.Sum256([]byte(run.ObservedContainerID + "\x00" + run.ObservedContainerStartedAt))
	identity := Identity{WorkspaceID: string(run.WorkspaceID), TaskID: string(run.TaskID), AttemptID: string(run.AttemptID),
		Generation: run.Generation, WriterGeneration: run.WriterGeneration, SessionID: string(run.OpenCodeSessionID),
		RuntimeEpoch: run.RuntimeEpoch, ContainerID: run.ObservedContainerID,
		StartedAt: run.ObservedContainerStartedAt, RuntimeToken: hex.EncodeToString(digest[:])}
	return identity, true
}

func attachmentRequestAllowed(request *http.Request, sessionID string) bool {
	if request.URL.EscapedPath() != request.URL.Path || request.URL.Path == "" || strings.Contains(request.URL.Path, "//") {
		return false
	}
	if request.URL.Path == "/fern" || strings.HasPrefix(request.URL.Path, "/fern/") {
		return false
	}
	if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		return false
	}
	if !attachmentWorkspaceSelectionAllowed(request) {
		return false
	}
	segments := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	for _, segment := range segments {
		if strings.HasPrefix(segment, "ses_") && segment != sessionID {
			return false
		}
	}
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return attachmentReadAllowed(request.URL.Path, request.URL.Query(), sessionID)
	}
	if request.Method == http.MethodPost && len(segments) == 3 && segments[0] == "permission" && segments[2] == "reply" {
		return true
	}
	if request.Method == http.MethodPost && len(segments) == 3 && segments[0] == "question" && (segments[2] == "reply" || segments[2] == "reject") {
		return true
	}
	if request.Method == http.MethodPost && len(segments) == 1 && segments[0] == "log" {
		return true
	}
	sessionIndex := -1
	if len(segments) >= 2 && segments[0] == "session" {
		sessionIndex = 0
	} else if len(segments) >= 3 && segments[0] == "api" && segments[1] == "session" {
		sessionIndex = 1
	}
	if sessionIndex < 0 || segments[sessionIndex+1] != sessionID {
		return false
	}
	suffix := segments[sessionIndex+2:]
	if sessionIndex == 1 {
		if request.Method != http.MethodPost || len(suffix) == 0 {
			return false
		}
		if len(suffix) == 1 {
			switch suffix[0] {
			case "agent", "model", "prompt", "compact", "wait", "interrupt":
				return true
			}
		}
		return len(suffix) == 2 && suffix[0] == "revert" && (suffix[1] == "stage" || suffix[1] == "clear" || suffix[1] == "commit") ||
			len(suffix) == 3 && (suffix[0] == "permission" || suffix[0] == "question") && (suffix[2] == "reply" || suffix[2] == "reject")
	}
	if request.Method == http.MethodPatch {
		return len(suffix) == 0 || (len(suffix) == 4 && suffix[0] == "message" && suffix[2] == "part")
	}
	if request.Method != http.MethodPost {
		return false
	}
	if len(suffix) == 2 && suffix[0] == "permissions" {
		return true
	}
	if len(suffix) != 1 {
		return false
	}
	switch suffix[0] {
	case "abort", "summarize", "message", "prompt_async", "command", "shell", "revert", "unrevert":
		return true
	default:
		return false
	}
}

func (m *Manager) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	username, password, authenticated := request.BasicAuth()
	digest := sha256.Sum256([]byte(password))
	now := time.Now().UTC()
	m.mu.Lock()
	current := m.active
	if m.closing {
		m.mu.Unlock()
		http.Error(writer, "route unavailable", http.StatusServiceUnavailable)
		return
	}
	if current == nil {
		m.mu.Unlock()
		http.NotFound(writer, request)
		return
	}
	capability, valid := m.attachments[digest]
	if !authenticated || username != AttachmentUsername || !valid || !capability.expiresAt.After(now) || capability.identity != current.identity {
		if valid && !capability.expiresAt.After(now) {
			delete(m.attachments, digest)
		}
		m.mu.Unlock()
		writer.Header().Set("WWW-Authenticate", `Basic realm="fern-run"`)
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !attachmentRequestAllowed(request, capability.sessionID) {
		m.mu.Unlock()
		http.Error(writer, "attachment operation forbidden", http.StatusForbidden)
		return
	}
	current.inflight++
	m.mu.Unlock()
	request.Header.Del("Authorization")
	request.Header.Del("Cookie")
	defer func() {
		m.mu.Lock()
		current.inflight--
		if current.removed && current.inflight == 0 {
			select {
			case <-current.drained:
			default:
				close(current.drained)
			}
		}
		m.mu.Unlock()
	}()
	requestContext, cancel := context.WithDeadline(request.Context(), capability.expiresAt)
	requestContext = context.WithValue(requestContext, attachmentSessionContextKey{}, capability.sessionID)
	stop := context.AfterFunc(current.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	current.handler.ServeHTTP(writer, request.WithContext(requestContext))
}

func (m *Manager) reverseProxy(current *binding) http.Handler {
	origin := m.origin
	return &httputil.ReverseProxy{
		Transport: current.target.transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(current.target.endpoint)
			for name := range request.Out.Header {
				lower := strings.ToLower(name)
				if strings.EqualFold(name, "Forwarded") || strings.HasPrefix(lower, "x-forwarded-") || strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") {
					delete(request.Out.Header, name)
				}
			}
			request.Out.Host = origin.Host
			request.Out.Header.Set("X-Forwarded-Host", origin.Host)
			request.Out.Header.Set("X-Forwarded-Proto", "https")
			request.Out.Header.Set("X-Forwarded-Port", origin.Port())
		},
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			stripFernCookies(response.Header)
			sessionID, _ := response.Request.Context().Value(attachmentSessionContextKey{}).(string)
			return filterAttachmentResponse(response, sessionID)
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			writer.Header().Set("Cache-Control", "no-store")
			http.Error(writer, "upstream unavailable", http.StatusBadGateway)
		},
	}
}

func stripFernCookies(header http.Header) {
	values := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, value := range values {
		reserved := false
		for _, segment := range strings.FieldsFunc(value, func(character rune) bool { return character == ';' || character == ',' }) {
			name, _, found := strings.Cut(strings.TrimSpace(segment), "=")
			if found && (name == "__Host-fern_device" || name == "fern_device") {
				reserved = true
				break
			}
		}
		if reserved {
			continue
		}
		header.Add("Set-Cookie", value)
	}
}

func validateIdentity(identity Identity) error {
	token := sha256.Sum256([]byte(identity.ContainerID + "\x00" + identity.StartedAt))
	if identity.WorkspaceID == "" || identity.TaskID == "" || identity.AttemptID == "" || identity.Generation <= 0 || identity.WriterGeneration != 1 || identity.SessionID == "" ||
		identity.RuntimeEpoch <= 0 || identity.ContainerID == "" || identity.StartedAt == "" ||
		identity.RuntimeToken != hex.EncodeToString(token[:]) {
		return errors.New("complete route identity is required")
	}
	return nil
}

func routeEvidence(status string, identity Identity) string {
	value, _ := json.Marshal(struct {
		Effect           string `json:"effect"`
		Status           string `json:"status"`
		Task             string `json:"task"`
		Attempt          string `json:"attempt"`
		Generation       int64  `json:"generation"`
		WriterGeneration int64  `json:"writer_generation"`
		SessionID        string `json:"session_id"`
		RuntimeEpoch     int64  `json:"runtime_epoch"`
	}{"background_route", status, identity.TaskID, identity.AttemptID, identity.Generation, identity.WriterGeneration, identity.SessionID, identity.RuntimeEpoch})
	return string(value)
}

func (identity Identity) String() string {
	return fmt.Sprintf("%s/%s/%s/g%d/w%d/%s@%d", identity.WorkspaceID, identity.TaskID, identity.AttemptID,
		identity.Generation, identity.WriterGeneration, identity.SessionID, identity.RuntimeEpoch)
}
