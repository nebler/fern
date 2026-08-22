package opencodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxRequestBytes  = 512 << 10
	maxResponseBytes = 1 << 20
	maxListEntries   = 1000
	maxIDBytes       = 256
	maxCredential    = 4096
	maxPageLimit     = 500
	maxScanPages     = 1000
)

type Config struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
}

type Client struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

func New(config Config) (*Client, error) {
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Scheme != "http" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, ErrInvalidConfiguration
	}
	host := base.Hostname()
	ip := net.ParseIP(host)
	if host == "" || (host != "localhost" && (ip == nil || !ip.IsLoopback())) || base.Port() == "" {
		return nil, ErrInvalidConfiguration
	}
	port, err := strconv.Atoi(base.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, ErrInvalidConfiguration
	}
	if config.HTTPClient == nil || !validCredential(config.Username, false) || !validCredential(config.Password, true) {
		return nil, ErrInvalidConfiguration
	}
	copyClient := *config.HTTPClient
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base.Path = ""
	return &Client{
		baseURL:    strings.TrimRight(base.String(), "/"),
		username:   config.Username,
		password:   config.Password,
		httpClient: &copyClient,
	}, nil
}

func validCredential(value string, allowColon bool) bool {
	if value == "" || len(value) > maxCredential || (!allowColon && strings.Contains(value, ":")) {
		return false
	}
	for _, char := range []byte(value) {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func (client *Client) CreateOrReuseSession(ctx context.Context, request CreateSessionRequest) (Session, error) {
	if !validCreateSessionRequest(request) {
		return Session{}, ErrInvalidConfiguration
	}
	var result Session
	if err := client.jsonRequest(ctx, http.MethodPost, "/api/session", request, &result); err != nil {
		return Session{}, err
	}
	if !sessionMatchesRequest(result, request) {
		return Session{}, protocolError("session semantics mismatch")
	}
	return result, nil
}

func (client *Client) ReadSession(ctx context.Context, sessionID string) (Session, error) {
	if !validID(sessionID, "ses") {
		return Session{}, ErrInvalidConfiguration
	}
	var result Session
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID), nil, &result); err != nil {
		return Session{}, err
	}
	if result.ID == "" || result.ID != sessionID {
		return Session{}, protocolError("session identity mismatch")
	}
	return result, nil
}

func (client *Client) AdmitPrompt(ctx context.Context, sessionID string, request PromptRequest) (Admission, error) {
	if !validID(sessionID, "ses") || !validID(request.ID, "msg_") {
		return Admission{}, ErrInvalidConfiguration
	}
	if len(request.Text) > MaxPromptTextBytes {
		return Admission{}, ErrRequestTooLarge
	}
	if request.Text == "" || !utf8.ValidString(request.Text) {
		return Admission{}, ErrInvalidConfiguration
	}
	var result Admission
	if err := client.jsonRequest(ctx, http.MethodPost, sessionPath(sessionID)+"/prompt", request, &result); err != nil {
		return Admission{}, err
	}
	if result.ID == "" || result.ID != request.ID {
		return Admission{}, protocolError("message identity mismatch")
	}
	return result, nil
}

func (client *Client) ListInbox(ctx context.Context, sessionID string) ([]InboxItem, error) {
	if !validID(sessionID, "ses") {
		return nil, ErrInvalidConfiguration
	}
	var result []InboxItem
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/inbox", nil, &result); err != nil {
		return nil, err
	}
	if len(result) > maxListEntries {
		return nil, ErrResponseTooLarge
	}
	seen := make(map[string]struct{}, len(result))
	for _, item := range result {
		if !validID(item.ID, "msg_") {
			return nil, ErrInvalidResponse
		}
		if _, exists := seen[item.ID]; exists {
			return nil, protocolError("duplicate inbox message identity")
		}
		seen[item.ID] = struct{}{}
		if item.SessionID != sessionID || item.Type != "user" || item.Delivery != "steer" || item.TimeCreated <= 0 ||
			item.Payload.Text == "" || len(item.Payload.Text) > MaxPromptTextBytes || !utf8.ValidString(item.Payload.Text) {
			return nil, protocolError("invalid inbox message semantics")
		}
	}
	return result, nil
}

func (client *Client) Messages(ctx context.Context, sessionID, cursor string, limit int) (MessagePage, error) {
	if !validID(sessionID, "ses") || limit < 1 || limit > maxPageLimit || len(cursor) > maxCredential {
		return MessagePage{}, ErrInvalidConfiguration
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	} else {
		query.Set("order", "asc")
	}
	var result MessagePage
	if err := client.messageRequest(ctx, sessionPath(sessionID)+"/message?"+query.Encode(), &result); err != nil {
		return MessagePage{}, err
	}
	if len(result.Data) > limit || len(result.Data) > maxListEntries {
		return MessagePage{}, protocolError("message page exceeded limit")
	}
	seen := make(map[string]struct{}, len(result.Data))
	for _, message := range result.Data {
		if !validID(message.ID, "msg_") {
			return MessagePage{}, ErrInvalidResponse
		}
		if _, exists := seen[message.ID]; exists {
			return MessagePage{}, protocolError("duplicate message identity in page")
		}
		seen[message.ID] = struct{}{}
	}
	return result, nil
}

func (client *Client) ReadMessage(ctx context.Context, sessionID, messageID string) (Message, error) {
	if !validID(sessionID, "ses") || !validID(messageID, "msg_") {
		return Message{}, ErrInvalidConfiguration
	}
	var result Message
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/message/"+url.PathEscape(messageID), nil, &result); err != nil {
		return Message{}, err
	}
	if result.ID == "" || result.ID != messageID {
		return Message{}, protocolError("message identity mismatch")
	}
	return result, nil
}

// CancelInboxOnce removes one previously proven undelivered inbox item. The
// caller must reconcile any error; this mutation is never retried internally.
func (client *Client) CancelInboxOnce(ctx context.Context, sessionID, messageID string) error {
	if !validID(sessionID, "ses") || !validID(messageID, "msg_") {
		return ErrInvalidConfiguration
	}
	return client.emptyRequest(ctx, http.MethodDelete, sessionPath(sessionID)+"/inbox/"+url.PathEscape(messageID), nil)
}

func (client *Client) ActiveSessions(ctx context.Context) (ActiveSessions, error) {
	var result ActiveSessions
	if err := client.jsonRequest(ctx, http.MethodGet, "/api/session/active", nil, &result); err != nil {
		return nil, err
	}
	if len(result) > maxListEntries {
		return nil, ErrResponseTooLarge
	}
	for id, state := range result {
		if !validID(id, "ses") || state.Type == "" {
			return nil, ErrInvalidResponse
		}
	}
	return result, nil
}

// ListPermissions reads the pinned profile's finite current-state projection.
// The observed contract does not prove that pending permissions survive an
// OpenCode process or container restart.
func (client *Client) ListPermissions(ctx context.Context, sessionID string) ([]Permission, error) {
	if !validID(sessionID, "ses") {
		return nil, ErrInvalidConfiguration
	}
	var result []Permission
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/permission", nil, &result); err != nil {
		return nil, err
	}
	if len(result) > maxListEntries {
		return nil, ErrResponseTooLarge
	}
	seen := make(map[string][]byte, len(result))
	for _, permission := range result {
		if !validID(permission.ID, "per") || permission.SessionID == "" || permission.Action == "" {
			return nil, ErrInvalidResponse
		}
		if permission.SessionID != sessionID {
			return nil, protocolError("permission ownership mismatch")
		}
		if prior, exists := seen[permission.ID]; exists {
			if !bytes.Equal(prior, permission.Bytes()) {
				return nil, protocolError("duplicate permission identity has incompatible bytes")
			}
			return nil, protocolError("duplicate permission identity")
		}
		seen[permission.ID] = permission.Bytes()
	}
	return result, nil
}

// ReadPermission reads one live pending permission. It does not establish
// restart persistence for the pending object.
func (client *Client) ReadPermission(ctx context.Context, sessionID, permissionID string) (Permission, error) {
	if !validID(sessionID, "ses") || !validID(permissionID, "per") {
		return Permission{}, ErrInvalidConfiguration
	}
	var result Permission
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/permission/"+url.PathEscape(permissionID), nil, &result); err != nil {
		return Permission{}, err
	}
	if !validID(result.ID, "per") || result.SessionID == "" || result.Action == "" {
		return Permission{}, ErrInvalidResponse
	}
	if result.ID != permissionID {
		return Permission{}, protocolError("permission identity mismatch")
	}
	if result.SessionID != sessionID {
		return Permission{}, protocolError("permission ownership mismatch")
	}
	return result, nil
}

// ReplyPermissionOnce applies the only reply shape proven by the pinned
// harness. The mutation is attempted exactly once; a lost response must be
// reconciled by the caller rather than retried automatically.
func (client *Client) ReplyPermissionOnce(ctx context.Context, sessionID, permissionID string) error {
	if !validID(sessionID, "ses") || !validID(permissionID, "per") {
		return ErrInvalidConfiguration
	}
	request := struct {
		Reply string `json:"reply"`
	}{Reply: "once"}
	return client.emptyRequest(ctx, http.MethodPost, sessionPath(sessionID)+"/permission/"+url.PathEscape(permissionID)+"/reply", request)
}

func (client *Client) ListForms(ctx context.Context, sessionID string) ([]Form, error) {
	if !validID(sessionID, "ses") {
		return nil, ErrInvalidConfiguration
	}
	var result []Form
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/form", nil, &result); err != nil {
		return nil, err
	}
	if len(result) > maxListEntries {
		return nil, ErrResponseTooLarge
	}
	seen := make(map[string][]byte, len(result))
	for _, form := range result {
		if form.ID == "" || form.SessionID != sessionID {
			return nil, protocolError("form ownership mismatch")
		}
		if prior, exists := seen[form.ID]; exists {
			if !bytes.Equal(prior, form.Bytes()) {
				return nil, protocolError("duplicate form identity has incompatible bytes")
			}
			return nil, protocolError("duplicate form identity")
		}
		seen[form.ID] = form.Bytes()
	}
	return result, nil
}

// ReadForm reads one exact live form object. Forms are process-epoch state and
// disappearance must not be interpreted as a successful reply.
func (client *Client) ReadForm(ctx context.Context, sessionID, formID string) (Form, error) {
	if !validID(sessionID, "ses") || !validOpaqueID(formID) {
		return Form{}, ErrInvalidConfiguration
	}
	var result Form
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/form/"+url.PathEscape(formID), nil, &result); err != nil {
		return Form{}, err
	}
	if result.ID != formID {
		return Form{}, protocolError("form identity mismatch")
	}
	if result.SessionID != sessionID {
		return Form{}, protocolError("form ownership mismatch")
	}
	return result, nil
}

func (client *Client) ReadFormState(ctx context.Context, sessionID, formID string) (FormState, error) {
	if !validID(sessionID, "ses") || !validOpaqueID(formID) {
		return FormState{}, ErrInvalidConfiguration
	}
	var result FormState
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/form/"+url.PathEscape(formID)+"/state", nil, &result); err != nil {
		return FormState{}, err
	}
	if result.Status == "" {
		return FormState{}, ErrInvalidResponse
	}
	return result, nil
}

func (client *Client) ReplyForm(ctx context.Context, sessionID, formID string, request FormReplyRequest) error {
	answer := bytes.TrimSpace(request.Answer)
	if !validID(sessionID, "ses") || !validOpaqueID(formID) || len(answer) < 2 || answer[0] != '{' || !json.Valid(answer) {
		return ErrInvalidConfiguration
	}
	return client.emptyRequest(ctx, http.MethodPost, sessionPath(sessionID)+"/form/"+url.PathEscape(formID)+"/reply", request)
}

func (client *Client) ReadContext(ctx context.Context, sessionID string) (Context, error) {
	if !validID(sessionID, "ses") {
		return Context{}, ErrInvalidConfiguration
	}
	var result Context
	if err := client.jsonRequest(ctx, http.MethodGet, sessionPath(sessionID)+"/context", nil, &result); err != nil {
		return Context{}, err
	}
	return result, nil
}

func (client *Client) Interrupt(ctx context.Context, sessionID string) error {
	if !validID(sessionID, "ses") {
		return ErrInvalidConfiguration
	}
	return client.emptyRequest(ctx, http.MethodPost, sessionPath(sessionID)+"/interrupt?continue=false", nil)
}

func sessionPath(sessionID string) string { return "/api/session/" + url.PathEscape(sessionID) }

func validID(value, prefix string) bool {
	return len(value) <= maxIDBytes && strings.HasPrefix(value, prefix) && validOpaqueID(value)
}

func validOpaqueID(value string) bool {
	if value == "" || len(value) > maxIDBytes {
		return false
	}
	for _, char := range []byte(value) {
		if char <= 0x20 || char >= 0x7f || char == '/' || char == '?' || char == '#' {
			return false
		}
	}
	return true
}

func (client *Client) jsonRequest(ctx context.Context, method, path string, body, destination any) error {
	payload, status, err := client.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &StatusError{statusCode: status}
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := strictDecode(payload, &envelope); err != nil || len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return ErrInvalidResponse
	}
	if err := validateUniqueJSON(envelope.Data); err != nil {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(envelope.Data, destination); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

func (client *Client) messageRequest(ctx context.Context, path string, destination *MessagePage) error {
	payload, status, err := client.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &StatusError{statusCode: status}
	}
	var envelope struct {
		Data   []Message `json:"data"`
		Cursor struct {
			Next *string `json:"next"`
		} `json:"cursor"`
	}
	if err := strictDecode(payload, &envelope); err != nil || envelope.Data == nil {
		return ErrInvalidResponse
	}
	if envelope.Cursor.Next != nil && len(*envelope.Cursor.Next) > maxCredential {
		return ErrInvalidResponse
	}
	destination.Data = envelope.Data
	destination.NextCursor = envelope.Cursor.Next
	return nil
}

func (client *Client) emptyRequest(ctx context.Context, method, path string, body any) error {
	payload, status, err := client.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return &StatusError{statusCode: status}
	}
	if len(bytes.TrimSpace(payload)) != 0 {
		return ErrInvalidResponse
	}
	return nil
}

func (client *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	if client == nil || client.httpClient == nil || client.baseURL == "" {
		return nil, 0, ErrInvalidConfiguration
	}
	if ctx == nil {
		return nil, 0, ErrDeadlineRequired
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, 0, ErrDeadlineRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	var encoded []byte
	var reader io.Reader
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, 0, ErrInvalidConfiguration
		}
		if len(encoded) > maxRequestBytes {
			return nil, 0, ErrRequestTooLarge
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, reader)
	if err != nil {
		return nil, 0, ErrInvalidConfiguration
	}
	// Mutations are deliberately non-replayable even if a caller's transport
	// elects to use Request.GetBody for retry decisions.
	if method != http.MethodGet {
		request.GetBody = nil
	}
	request.Header.Set("Accept", "application/json")
	request.SetBasicAuth(client.username, client.password)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, 0, contextErr
		}
		return nil, 0, ErrRequestFailed
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, 0, ErrRequestFailed
	}
	if len(payload) > maxResponseBytes {
		return nil, 0, ErrResponseTooLarge
	}
	return payload, response.StatusCode, nil
}

func strictDecode(payload []byte, destination any) error {
	if err := validateUniqueJSON(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalidResponse
	}
	return nil
}

func validateUniqueJSON(payload []byte) error {
	if len(payload) == 0 || !utf8.Valid(payload) {
		return ErrInvalidResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := validateJSONToken(decoder, 0); err != nil {
		return ErrInvalidResponse
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return ErrInvalidResponse
	}
	return nil
}

func validateJSONToken(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrInvalidResponse
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return ErrInvalidResponse
			}
			canonical := strings.ToLower(key)
			if _, exists := keys[canonical]; exists {
				return ErrInvalidResponse
			}
			keys[canonical] = struct{}{}
			if err := validateJSONToken(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalidResponse
		}
	case '[':
		for decoder.More() {
			if err := validateJSONToken(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalidResponse
		}
	default:
		return ErrInvalidResponse
	}
	return nil
}
