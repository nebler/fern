package githubapp

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/atomicfile"
	"github.com/nebler/fern/internal/strictjson"
)

const (
	credentialStoreVersion = 2
	credentialFileName     = "app-credentials.json"
	maxCredentialFileBytes = 128 << 10
)

var (
	ErrCredentialStoreSecurity  = errors.New("GitHub App credential store has unsafe filesystem permissions or type")
	ErrCredentialStoreIO        = errors.New("GitHub App credential store operation failed")
	ErrCredentialsNotFound      = errors.New("GitHub App credentials not found")
	ErrStoredCredentialsInvalid = errors.New("stored GitHub App credentials are invalid")
)

// AppCredentials is a GitHub App ID and its validated RSA private key. Its
// formatting methods always redact the key.
type AppCredentials struct {
	appID         int64
	privateKeyPEM []byte
	privateKey    *rsa.PrivateKey
}

// NewAppCredentials validates an App ID and its PEM-encoded private key as
// downloaded from the App's GitHub settings page.
func NewAppCredentials(appID int64, privateKeyPEM []byte) (AppCredentials, error) {
	if appID <= 0 {
		return AppCredentials{}, ErrInvalidConfiguration
	}
	key, err := ParseRSAPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return AppCredentials{}, err
	}
	return AppCredentials{appID: appID, privateKeyPEM: bytes.Clone(privateKeyPEM), privateKey: key}, nil
}

func (credentials AppCredentials) AppID() int64 { return credentials.appID }

func (credentials AppCredentials) PrivateKey() *rsa.PrivateKey { return credentials.privateKey }

func (credentials AppCredentials) String() string {
	return fmt.Sprintf("GitHub App credentials (app ID %d; private key redacted)", credentials.appID)
}

func (credentials AppCredentials) GoString() string { return credentials.String() }

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
		return nil, fmt.Errorf("%w: %v", ErrCredentialStoreIO, err)
	}
	return &CredentialStore{directory: directory}, nil
}

// Save atomically replaces the stored credentials. Readers see either the
// complete old or complete new file. Errors never include credential content.
func (store *CredentialStore) Save(credentials AppCredentials) error {
	if store == nil || store.directory == "" {
		return ErrCredentialStoreSecurity
	}
	payload, err := marshalStoredCredentials(credentials)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(store.path(), payload, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrCredentialStoreIO, err)
	}
	return nil
}

func (store *CredentialStore) path() string {
	return filepath.Join(store.directory, credentialFileName)
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
		return AppCredentials{}, fmt.Errorf("%w: %v", ErrCredentialStoreIO, err)
	}
	return parseStoredCredentials(payload)
}

type storedCredentialFile struct {
	Version       int    `json:"version"`
	AppID         int64  `json:"app_id"`
	PrivateKeyPEM string `json:"private_key_pem"`
}

// marshalStoredCredentials returns the strict store representation.
func marshalStoredCredentials(credentials AppCredentials) ([]byte, error) {
	if _, err := NewAppCredentials(credentials.appID, credentials.privateKeyPEM); err != nil {
		return nil, ErrStoredCredentialsInvalid
	}
	payload, err := json.Marshal(storedCredentialFile{
		Version:       credentialStoreVersion,
		AppID:         credentials.appID,
		PrivateKeyPEM: string(credentials.privateKeyPEM),
	})
	if err != nil {
		return nil, ErrStoredCredentialsInvalid
	}
	return append(payload, '\n'), nil
}

// parseStoredCredentials strictly validates a store payload.
func parseStoredCredentials(payload []byte) (AppCredentials, error) {
	if len(payload) > maxCredentialFileBytes || strictjson.Check(payload, 4) != nil {
		return AppCredentials{}, ErrStoredCredentialsInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded storedCredentialFile
	if err := decoder.Decode(&decoded); err != nil || decoded.Version != credentialStoreVersion {
		return AppCredentials{}, ErrStoredCredentialsInvalid
	}
	credentials, err := NewAppCredentials(decoded.AppID, []byte(decoded.PrivateKeyPEM))
	if err != nil {
		return AppCredentials{}, ErrStoredCredentialsInvalid
	}
	return credentials, nil
}
