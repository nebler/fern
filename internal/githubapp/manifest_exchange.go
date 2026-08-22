package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

const (
	maxManifestCodeBytes     = 256
	maxManifestResponseBytes = 64 << 10
	maxManifestSecretBytes   = 8 << 10
)

type manifestCodeState struct {
	value string
	used  atomic.Bool
}

// ManifestCode is a bounded at-most-once credential returned to the callback.
// Copies share consumption state.
type ManifestCode struct {
	state *manifestCodeState
}

func NewManifestCode(value string) (ManifestCode, error) {
	if !validManifestCode(value) {
		return ManifestCode{}, ErrInvalidManifestCode
	}
	return ManifestCode{state: &manifestCodeState{value: value}}, nil
}

func (code ManifestCode) String() string {
	return "GitHub App manifest code (redacted)"
}

func (code ManifestCode) GoString() string {
	return code.String()
}

// ManifestClient exchanges manifest callback codes for host credentials.
type ManifestClient struct {
	httpClient *http.Client
	apiBase    string
}

func NewManifestClient(httpClient *http.Client) (*ManifestClient, error) {
	if httpClient == nil {
		return nil, ErrInvalidConfiguration
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &ManifestClient{httpClient: &clientCopy, apiBase: githubAPIBase}, nil
}

// AppCredentials contains values that must be persisted by the host before the
// manifest callback completes. Its formatting methods always redact secrets.
type AppCredentials struct {
	appID         int64
	clientID      string
	clientSecret  string
	webhookSecret string
	privateKeyPEM []byte
	privateKey    *rsa.PrivateKey
}

func (credentials AppCredentials) AppID() int64 {
	return credentials.appID
}

func (credentials AppCredentials) ClientID() string {
	return credentials.clientID
}

func (credentials AppCredentials) ClientSecret() string {
	return credentials.clientSecret
}

func (credentials AppCredentials) WebhookSecret() string {
	return credentials.webhookSecret
}

func (credentials AppCredentials) PrivateKeyPEM() []byte {
	return bytes.Clone(credentials.privateKeyPEM)
}

func (credentials AppCredentials) PrivateKey() *rsa.PrivateKey {
	return credentials.privateKey
}

func (credentials AppCredentials) String() string {
	if credentials.appID <= 0 {
		return "GitHub App credentials (redacted)"
	}
	return fmt.Sprintf("GitHub App credentials (app ID %d; secrets redacted)", credentials.appID)
}

func (credentials AppCredentials) GoString() string {
	return credentials.String()
}

func (client *ManifestClient) Exchange(ctx context.Context, code ManifestCode) (AppCredentials, error) {
	if client == nil || client.httpClient == nil || client.apiBase == "" {
		return AppCredentials{}, ErrInvalidConfiguration
	}
	if _, ok := ctx.Deadline(); !ok {
		return AppCredentials{}, ErrDeadlineRequired
	}
	if err := ctx.Err(); err != nil {
		return AppCredentials{}, err
	}
	if code.state == nil || !validManifestCode(code.state.value) {
		return AppCredentials{}, ErrInvalidManifestCode
	}
	if !code.state.used.CompareAndSwap(false, true) {
		return AppCredentials{}, ErrManifestCodeUsed
	}

	endpoint := strings.TrimRight(client.apiBase, "/") + "/app-manifests/" + code.state.value + "/conversions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return AppCredentials{}, ErrInvalidConfiguration
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "fern-githubapp")

	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return AppCredentials{}, ctxErr
		}
		return AppCredentials{}, ErrRequestFailed
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxManifestResponseBytes+1))
	if err != nil {
		return AppCredentials{}, ErrRequestFailed
	}
	if len(payload) > maxManifestResponseBytes {
		return AppCredentials{}, ErrResponseTooLarge
	}
	if response.StatusCode != http.StatusCreated {
		return AppCredentials{}, &HTTPError{statusCode: response.StatusCode}
	}

	var decoded manifestConversionResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil {
		return AppCredentials{}, ErrInvalidAppCredentials
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AppCredentials{}, ErrInvalidAppCredentials
	}
	key, err := ParseRSAPrivateKeyPEM([]byte(decoded.PEM))
	if err != nil || decoded.ID <= 0 || !validManifestSecret(decoded.ClientID, 1, 512) || !validManifestSecret(decoded.ClientSecret, 1, maxManifestSecretBytes) || !validManifestSecret(decoded.WebhookSecret, 0, maxManifestSecretBytes) {
		return AppCredentials{}, ErrInvalidAppCredentials
	}
	return AppCredentials{
		appID:         decoded.ID,
		clientID:      decoded.ClientID,
		clientSecret:  decoded.ClientSecret,
		webhookSecret: decoded.WebhookSecret,
		privateKeyPEM: []byte(decoded.PEM),
		privateKey:    key,
	}, nil
}

type manifestConversionResponse struct {
	ID                 int64             `json:"id"`
	Slug               string            `json:"slug"`
	NodeID             string            `json:"node_id"`
	Owner              json.RawMessage   `json:"owner"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	ExternalURL        string            `json:"external_url"`
	HTMLURL            string            `json:"html_url"`
	CreatedAt          string            `json:"created_at"`
	UpdatedAt          string            `json:"updated_at"`
	Permissions        map[string]string `json:"permissions"`
	Events             []string          `json:"events"`
	InstallationsCount int               `json:"installations_count"`
	ClientID           string            `json:"client_id"`
	ClientSecret       string            `json:"client_secret"`
	WebhookSecret      string            `json:"webhook_secret"`
	PEM                string            `json:"pem"`
}

func validManifestCode(value string) bool {
	if value == "" || len(value) > maxManifestCodeBytes {
		return false
	}
	for _, char := range []byte(value) {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validManifestSecret(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, char := range []byte(value) {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}
