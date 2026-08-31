package backgroundopencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/jsoncanon"
)

type Config struct {
	Endpoint   string
	Username   string
	Password   string
	HTTPClient *http.Client
}

type Client struct {
	endpoint string
	username string
	password string
	http     *http.Client
}

func New(config Config) (*Client, error) {
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path != "" || parsed.Hostname() == "" || parsed.Port() == "" {
		return nil, ErrInvalidConfig
	}
	ip := net.ParseIP(parsed.Hostname())
	port, portErr := strconv.Atoi(parsed.Port())
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 || parsed.String() != config.Endpoint {
		return nil, ErrInvalidConfig
	}
	if !validCredential(config.Username, false) || !validCredential(config.Password, true) || config.HTTPClient == nil || config.HTTPClient.Timeout <= 0 || config.HTTPClient.Timeout > 30*time.Second {
		return nil, ErrInvalidConfig
	}
	copyClient := *config.HTTPClient
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: config.Endpoint, username: config.Username, password: config.Password, http: &copyClient}, nil
}

// CreateSessionOnce issues at most one POST and never reconciles or retries it.
func (c *Client) CreateSessionOnce(ctx context.Context, spec SessionSpec) error {
	if !validSessionSpec(spec) {
		return ErrInvalidConfig
	}
	body := struct {
		ID       string      `json:"id"`
		Agent    string      `json:"agent"`
		Model    modelRef    `json:"model"`
		Location locationRef `json:"location"`
	}{spec.ID, spec.Agent, modelRef{ID: spec.ModelID, ProviderID: spec.ProviderID}, locationRef{Directory: spec.Directory}}
	var envelope struct {
		Data sessionInfo `json:"data"`
	}
	if err := c.json(ctx, http.MethodPost, "/api/session", body, http.StatusOK, "create session", statusAuthority{}, &envelope); err != nil {
		return err
	}
	return validateSession(envelope.Data, spec, "create session")
}

func (c *Client) ReadSession(ctx context.Context, sessionID string) (sessionInfo, error) {
	if !validSessionID(sessionID) {
		return sessionInfo{}, ErrInvalidConfig
	}
	var envelope struct {
		Data sessionInfo `json:"data"`
	}
	if err := c.json(ctx, http.MethodGet, sessionPath(sessionID), nil, http.StatusOK, "read session", statusAuthority{sessionID: sessionID}, &envelope); err != nil {
		return sessionInfo{}, err
	}
	if envelope.Data.ID != sessionID {
		return sessionInfo{}, protocol("read session", "identity mismatch")
	}
	if err := validateSessionShape(envelope.Data, "read session"); err != nil {
		return sessionInfo{}, err
	}
	return envelope.Data, nil
}

// AdmitPromptOnce issues at most one POST. In particular, a lost response is
// returned as transport ambiguity and never causes an internal replay.
func (c *Client) AdmitPromptOnce(ctx context.Context, sessionID string, spec PromptSpec) error {
	if !validSessionID(sessionID) || !validPromptSpec(spec) {
		return ErrInvalidConfig
	}
	var envelope struct {
		Data admission `json:"data"`
	}
	body := promptBody{ID: spec.ID, Prompt: promptText{Text: spec.Text}, Delivery: spec.Delivery, Resume: spec.Resume}
	if err := c.json(ctx, http.MethodPost, sessionPath(sessionID)+"/prompt", body, http.StatusOK, "admit prompt", statusAuthority{sessionID: sessionID, conflictResource: spec.ID}, &envelope); err != nil {
		return err
	}
	got := envelope.Data
	if got.AdmittedSeq == nil || *got.AdmittedSeq < 1 || got.ID != spec.ID || got.SessionID != sessionID || got.Prompt.Text != spec.Text || len(got.Prompt.Files) != 0 || len(got.Prompt.Agents) != 0 || got.Delivery != spec.Delivery || got.TimeCreated == nil || !finiteNonnegative(*got.TimeCreated) {
		return protocol("admit prompt", "admission semantics mismatch")
	}
	if got.PromotedSeq != nil && *got.PromotedSeq <= *got.AdmittedSeq {
		return protocol("admit prompt", "promotion sequence does not follow admission")
	}
	return nil
}

// InterruptOnce is one non-replayable mutation. A transport error is ambiguous.
func (c *Client) InterruptOnce(ctx context.Context, sessionID string) error {
	if !validSessionID(sessionID) {
		return ErrInvalidConfig
	}
	return c.empty(ctx, http.MethodPost, sessionPath(sessionID)+"/interrupt", "interrupt", statusAuthority{sessionID: sessionID})
}

type statusAuthority struct {
	sessionID        string
	conflictResource string
}

func (c *Client) json(ctx context.Context, method, requestPath string, body any, status int, operation string, authority statusAuthority, destination any) error {
	payload, gotStatus, headers, err := c.do(ctx, method, requestPath, body, operation)
	if err != nil {
		return err
	}
	if gotStatus != status {
		return classifyStatus(operation, gotStatus, payload, headers, authority)
	}
	if err := exactJSONContentType(headers); err != nil {
		return protocol(operation, "content type")
	}
	return strictDecode(payload, destination, operation)
}

func (c *Client) empty(ctx context.Context, method, requestPath, operation string, authority statusAuthority) error {
	payload, status, headers, err := c.do(ctx, method, requestPath, nil, operation)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return classifyStatus(operation, status, payload, headers, authority)
	}
	if len(payload) != 0 || len(headers.Values("Content-Type")) != 0 {
		return protocol(operation, "204 response is not exactly empty")
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, requestPath string, body any, operation string) ([]byte, int, http.Header, error) {
	if c == nil || c.http == nil || c.endpoint == "" {
		return nil, 0, nil, ErrInvalidConfig
	}
	if ctx == nil {
		return nil, 0, nil, ErrDeadline
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, 0, nil, ErrDeadline
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, nil, err
	}
	var encoded []byte
	var reader io.Reader
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil || len(encoded) > maxRequestBytes {
			return nil, 0, nil, ErrInvalidConfig
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint+requestPath, reader)
	if err != nil {
		return nil, 0, nil, ErrInvalidConfig
	}
	if method != http.MethodGet {
		request.GetBody = nil
	}
	request.Header.Set("Accept", "application/json")
	request.SetBasicAuth(c.username, c.password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// This is the last point at which no network dispatch has begun. After the
	// call below starts, every transport failure is ambiguous to the caller.
	if err := ctx.Err(); err != nil {
		return nil, 0, nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, 0, nil, &TransportError{operation: operation, kind: "request"}
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, 0, nil, &TransportError{operation: operation, kind: "response"}
	}
	if len(payload) > maxResponseBytes {
		return nil, 0, nil, protocol(operation, "response body exceeds bound")
	}
	return payload, response.StatusCode, response.Header.Clone(), nil
}

type sessionNotFoundBody struct {
	Tag       string `json:"_tag"`
	SessionID string `json:"sessionID"`
	Message   string `json:"message"`
}

type conflictBody struct {
	Tag      string `json:"_tag"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
}

func classifyStatus(operation string, status int, payload []byte, headers http.Header, authority statusAuthority) error {
	if err := exactJSONContentType(headers); err != nil {
		return protocol(operation, "error content type")
	}
	switch status {
	case http.StatusNotFound:
		if authority.sessionID == "" {
			return protocol(operation, "unauthorized not-found status")
		}
		var body sessionNotFoundBody
		if err := strictDecode(payload, &body, operation+" error"); err != nil {
			return err
		}
		if body.Tag != "SessionNotFoundError" || body.SessionID != authority.sessionID || body.Message != "Session not found: "+authority.sessionID {
			return protocol(operation, "not-found authority mismatch")
		}
		return &NotFoundError{operation: operation}
	case http.StatusConflict:
		if authority.conflictResource == "" {
			return protocol(operation, "unauthorized conflict status")
		}
		var body conflictBody
		if err := strictDecode(payload, &body, operation+" error"); err != nil {
			return err
		}
		if body.Tag != "ConflictError" || body.Resource != authority.conflictResource || body.Message != "Prompt message ID conflicts with an existing durable record: "+authority.conflictResource {
			return protocol(operation, "conflict authority mismatch")
		}
		return &ConflictError{operation: operation}
	default:
		return protocol(operation, "unexpected HTTP status")
	}
}

func exactJSONContentType(header http.Header) error {
	values := header.Values("Content-Type")
	if len(values) != 1 {
		return ErrProtocol
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" || len(parameters) != 0 || values[0] != "application/json" {
		return ErrProtocol
	}
	return nil
}

func strictDecode(payload []byte, destination any, operation string) error {
	if len(payload) == 0 || jsoncanon.Check(payload, maxJSONDepth) != nil {
		return protocol(operation, "invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return protocol(operation, "schema mismatch")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return protocol(operation, "trailing JSON")
	}
	return nil
}

func validateSession(info sessionInfo, spec SessionSpec, operation string) error {
	if err := validateSessionShape(info, operation); err != nil {
		return err
	}
	if info.ID != spec.ID || info.Agent != spec.Agent || info.Model == nil || info.Model.ProviderID != spec.ProviderID || info.Model.ID != spec.ModelID || info.Model.Variant != "default" || info.Location == nil || info.Location.Directory != spec.Directory || info.Location.WorkspaceID != "" {
		return protocol(operation, "session semantics mismatch")
	}
	return nil
}

func validateSessionShape(info sessionInfo, operation string) error {
	if !validSessionID(info.ID) || !validOpaque(info.ProjectID, 512) || info.Title == "" || len(info.Title) > 4096 || !utf8.ValidString(info.Title) || info.Cost == nil || !finiteNonnegative(*info.Cost) || info.Time == nil || info.Time.Created == nil || info.Time.Updated == nil || !finiteNonnegative(*info.Time.Created) || !finiteNonnegative(*info.Time.Updated) || *info.Time.Updated < *info.Time.Created || info.Location == nil || !validLocation(info.Location.Directory) {
		return protocol(operation, "invalid session shape")
	}
	if info.Tokens == nil || info.Tokens.Input == nil || info.Tokens.Output == nil || info.Tokens.Reasoning == nil || info.Tokens.Cache == nil || info.Tokens.Cache.Read == nil || info.Tokens.Cache.Write == nil {
		return protocol(operation, "missing session counters")
	}
	values := []float64{*info.Tokens.Input, *info.Tokens.Output, *info.Tokens.Reasoning, *info.Tokens.Cache.Read, *info.Tokens.Cache.Write}
	for _, value := range values {
		if !finiteNonnegative(value) {
			return protocol(operation, "invalid session counters")
		}
	}
	if info.Agent != "" && !validToken(info.Agent, 128) {
		return protocol(operation, "invalid agent")
	}
	if info.Model != nil && (!validToken(info.Model.ProviderID, 128) || !validToken(info.Model.ID, 256) || (info.Model.Variant != "" && !validToken(info.Model.Variant, 128))) {
		return protocol(operation, "invalid model")
	}
	if info.ParentID != "" && !validSessionID(info.ParentID) {
		return protocol(operation, "invalid parent identity")
	}
	if info.Revert != nil {
		if !validMessageID(info.Revert.MessageID) || (info.Revert.PartID != "" && !validOpaque(info.Revert.PartID, maxIDBytes)) || len(info.Revert.Files) > 10000 {
			return protocol(operation, "invalid revert")
		}
		for _, file := range info.Revert.Files {
			if file.Path == "" || (file.Status != "added" && file.Status != "modified" && file.Status != "deleted") || !finiteNonnegative(file.Additions) || !finiteNonnegative(file.Deletions) {
				return protocol(operation, "invalid revert file")
			}
		}
	}
	return nil
}

func validSessionSpec(spec SessionSpec) bool {
	return validSessionID(spec.ID) && validToken(spec.Agent, 128) && validToken(spec.ProviderID, 128) && validToken(spec.ModelID, 256) && validLocation(spec.Directory)
}

func validPromptSpec(spec PromptSpec) bool {
	return validMessageID(spec.ID) && spec.Text != "" && len(spec.Text) <= maxPromptBytes && utf8.ValidString(spec.Text) && spec.Delivery == "steer"
}

func validLocation(value string) bool {
	return len(value) >= 2 && len(value) <= 4096 && strings.HasPrefix(value, "/") && path.Clean(value) == value && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validSessionID(value string) bool { return validPrefixedID(value, "ses_") }
func validMessageID(value string) bool { return validPrefixedID(value, "msg_") }

func validPrefixedID(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && validOpaque(value, maxIDBytes)
}

func validOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for i := range len(value) {
		if value[i] <= 0x20 || value[i] >= 0x7f || strings.ContainsRune("/?#\\", rune(value[i])) {
			return false
		}
	}
	return true
}

func validToken(value string, maximum int) bool {
	return validOpaque(value, maximum) && !strings.Contains(value, ":")
}

func validCredential(value string, colon bool) bool {
	if value == "" || len(value) > maxCredential || (!colon && strings.Contains(value, ":")) {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x20 || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func sessionPath(sessionID string) string { return "/api/session/" + url.PathEscape(sessionID) }
