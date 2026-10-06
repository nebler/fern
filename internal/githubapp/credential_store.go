package githubapp

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/atomicfile"
)

const (
	credentialStoreVersion = 1
	credentialFileName     = "app-credentials.json"
	maxCredentialFileBytes = 128 << 10
)

var (
	ErrCredentialStoreSecurity  = errors.New("GitHub App credential store has unsafe filesystem permissions or type")
	ErrCredentialStoreIO        = errors.New("GitHub App credential store operation failed")
	ErrCredentialsNotFound      = errors.New("GitHub App credentials not found")
	ErrStoredCredentialsInvalid = errors.New("stored GitHub App credentials are invalid")
)

// CredentialStore persists host-only GitHub App credentials in a caller-owned,
// dedicated private directory (see atomicfile.PrivateDir). The credential file
// is written with mode 0600.
//
// This store provides filesystem-permission protection, not encryption at rest.
type CredentialStore struct {
	directory string
}

// NewCredentialStore creates or validates a dedicated credential directory.
func NewCredentialStore(directory string) (*CredentialStore, error) {
	if directory == "" {
		return nil, ErrCredentialStoreSecurity
	}
	if err := atomicfile.PrivateDir(directory); err != nil {
		if errors.Is(err, atomicfile.ErrUnsafeDir) {
			return nil, ErrCredentialStoreSecurity
		}
		return nil, ErrCredentialStoreIO
	}
	return &CredentialStore{directory: directory}, nil
}

// Save atomically replaces the stored credentials. Readers see either the
// complete old or complete new file. Errors are static and never include
// credential content.
func (store *CredentialStore) Save(credentials AppCredentials) error {
	if store == nil || store.directory == "" {
		return ErrCredentialStoreSecurity
	}
	payload, err := MarshalStoredCredentials(credentials)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(store.path(), payload, 0o600); err != nil {
		return ErrCredentialStoreIO
	}
	return nil
}

// Delete removes the committed generation and durably records its absence.
// It is idempotent so a failed bootstrap can restore the empty-store state
// whether Save failed before or after making the candidate visible.
func (store *CredentialStore) Delete() error {
	if store == nil || store.directory == "" {
		return ErrCredentialStoreSecurity
	}
	if err := os.Remove(store.path()); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return ErrCredentialStoreIO
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return ErrCredentialStoreIO
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return ErrCredentialStoreIO
	}
	return nil
}

func (store *CredentialStore) path() string {
	return filepath.Join(store.directory, credentialFileName)
}

// MarshalStoredCredentials returns the strict store representation for direct
// encryption. Callers must not persist the returned plaintext.
func MarshalStoredCredentials(credentials AppCredentials) ([]byte, error) {
	if _, err := validateStoredCredentialValues(credentials.appID, credentials.clientID, credentials.clientSecret, credentials.webhookSecret, credentials.privateKeyPEM); err != nil {
		return nil, ErrStoredCredentialsInvalid
	}
	payload, err := json.Marshal(storedCredentialFile{
		Version:       credentialStoreVersion,
		AppID:         credentials.appID,
		ClientID:      credentials.clientID,
		ClientSecret:  credentials.clientSecret,
		WebhookSecret: credentials.webhookSecret,
		PrivateKeyPEM: string(credentials.privateKeyPEM),
	})
	if err != nil {
		return nil, ErrStoredCredentialsInvalid
	}
	payload = append(payload, '\n')
	if len(payload) > maxCredentialFileBytes {
		return nil, ErrStoredCredentialsInvalid
	}
	return payload, nil
}

// Load reads and validates the complete committed credential file.
func (store *CredentialStore) Load() (AppCredentials, error) {
	if store == nil || store.directory == "" {
		return AppCredentials{}, ErrCredentialStoreSecurity
	}
	payload, err := atomicfile.Read(store.path(), maxCredentialFileBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return AppCredentials{}, ErrCredentialsNotFound
	case errors.Is(err, atomicfile.ErrTooLarge):
		return AppCredentials{}, ErrStoredCredentialsInvalid
	case err != nil:
		return AppCredentials{}, ErrCredentialStoreIO
	}
	return ParseStoredCredentials(payload)
}

// ParseStoredCredentials strictly validates a decrypted in-memory candidate.
func ParseStoredCredentials(payload []byte) (AppCredentials, error) {
	if len(payload) > maxCredentialFileBytes {
		return AppCredentials{}, ErrStoredCredentialsInvalid
	}
	decoded, err := decodeStoredCredentialFile(payload)
	if err != nil {
		return AppCredentials{}, ErrStoredCredentialsInvalid
	}
	key, err := validateStoredCredentialValues(decoded.AppID, decoded.ClientID, decoded.ClientSecret, decoded.WebhookSecret, []byte(decoded.PrivateKeyPEM))
	if err != nil {
		return AppCredentials{}, ErrStoredCredentialsInvalid
	}
	return AppCredentials{
		appID:         decoded.AppID,
		clientID:      decoded.ClientID,
		clientSecret:  decoded.ClientSecret,
		webhookSecret: decoded.WebhookSecret,
		privateKeyPEM: []byte(decoded.PrivateKeyPEM),
		privateKey:    key,
	}, nil
}

type storedCredentialFile struct {
	Version       int    `json:"version"`
	AppID         int64  `json:"app_id"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PrivateKeyPEM string `json:"private_key_pem"`
}

func decodeStoredCredentialFile(payload []byte) (storedCredentialFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return storedCredentialFile{}, ErrStoredCredentialsInvalid
	}
	var decoded storedCredentialFile
	seen := make(map[string]bool, 6)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return storedCredentialFile{}, ErrStoredCredentialsInvalid
		}
		seen[name] = true
		switch name {
		case "version":
			err = decoder.Decode(&decoded.Version)
		case "app_id":
			err = decoder.Decode(&decoded.AppID)
		case "client_id":
			err = decoder.Decode(&decoded.ClientID)
		case "client_secret":
			err = decoder.Decode(&decoded.ClientSecret)
		case "webhook_secret":
			err = decoder.Decode(&decoded.WebhookSecret)
		case "private_key_pem":
			err = decoder.Decode(&decoded.PrivateKeyPEM)
		default:
			return storedCredentialFile{}, ErrStoredCredentialsInvalid
		}
		if err != nil {
			return storedCredentialFile{}, ErrStoredCredentialsInvalid
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 6 || decoded.Version != credentialStoreVersion {
		return storedCredentialFile{}, ErrStoredCredentialsInvalid
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return storedCredentialFile{}, ErrStoredCredentialsInvalid
	}
	return decoded, nil
}

func validateStoredCredentialValues(appID int64, clientID, clientSecret, webhookSecret string, privateKeyPEM []byte) (*rsa.PrivateKey, error) {
	key, err := ParseRSAPrivateKeyPEM(privateKeyPEM)
	if err != nil || appID <= 0 || !validManifestSecret(clientID, 1, 512) || !validManifestSecret(clientSecret, 1, maxManifestSecretBytes) || !validManifestSecret(webhookSecret, 0, maxManifestSecretBytes) {
		return nil, ErrStoredCredentialsInvalid
	}
	return key, nil
}
