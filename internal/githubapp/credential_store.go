package githubapp

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const (
	credentialStoreVersion  = 1
	credentialFileName      = "app-credentials.json"
	maxCredentialFileBytes  = 128 << 10
	credentialTemporaryStem = ".app-credentials-"
)

var (
	ErrCredentialStoreSecurity  = errors.New("GitHub App credential store has unsafe filesystem permissions or type")
	ErrCredentialStoreIO        = errors.New("GitHub App credential store operation failed")
	ErrCredentialsNotFound      = errors.New("GitHub App credentials not found")
	ErrStoredCredentialsInvalid = errors.New("stored GitHub App credentials are invalid")
)

// CredentialStore persists host-only GitHub App credentials in a caller-owned,
// dedicated directory. It creates the directory with mode 0700 and credentials
// with mode 0600, and refuses symlinks or less restrictive filesystem objects.
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
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, ErrCredentialStoreIO
	}
	handle, err := openCredentialDirectory(directory)
	if err != nil {
		return nil, err
	}
	if err := handle.Close(); err != nil {
		return nil, ErrCredentialStoreIO
	}
	return &CredentialStore{directory: directory}, nil
}

func (store *CredentialStore) String() string {
	return "GitHub App credential store"
}

func (store *CredentialStore) GoString() string {
	return store.String()
}

// Save atomically replaces the stored credentials after the temporary file is
// durable. Readers see either the complete old or complete new file. A failure
// before rename leaves the old file untouched; a directory sync failure is
// reported after the complete replacement has become visible.
func (store *CredentialStore) Save(credentials AppCredentials) error {
	if store == nil || store.directory == "" {
		return ErrCredentialStoreSecurity
	}
	if _, err := validateStoredCredentialValues(credentials.appID, credentials.clientID, credentials.clientSecret, credentials.webhookSecret, credentials.privateKeyPEM); err != nil {
		return ErrStoredCredentialsInvalid
	}

	payload, err := MarshalStoredCredentials(credentials)
	if err != nil {
		return err
	}

	directory, err := openCredentialDirectory(store.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	if _, err := inspectCredentialEntry(int(directory.Fd())); err != nil {
		return err
	}

	temporary, temporaryName, err := createCredentialTemporary(int(directory.Fd()))
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(directory.Fd()), temporaryName, 0)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return ErrCredentialStoreIO
	}
	if err := requirePrivateRegularFile(temporary); err != nil {
		temporary.Close()
		return err
	}
	if written, err := temporary.Write(payload); err != nil || written != len(payload) {
		temporary.Close()
		return ErrCredentialStoreIO
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return ErrCredentialStoreIO
	}
	if err := temporary.Close(); err != nil {
		return ErrCredentialStoreIO
	}

	// Recheck immediately before replacement so an unsafe target is never
	// intentionally accepted, even if it appeared while the temp file was made.
	if _, err := inspectCredentialEntry(int(directory.Fd())); err != nil {
		return err
	}
	if err := unix.Renameat(int(directory.Fd()), temporaryName, int(directory.Fd()), credentialFileName); err != nil {
		return ErrCredentialStoreIO
	}
	if err := directory.Sync(); err != nil {
		// The replacement is complete, but its persistence across a host crash is
		// uncertain. The static error deliberately exposes no credential content.
		return ErrCredentialStoreIO
	}
	return nil
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
	directory, err := openCredentialDirectory(store.directory)
	if err != nil {
		return AppCredentials{}, err
	}
	defer directory.Close()

	exists, err := inspectCredentialEntry(int(directory.Fd()))
	if err != nil {
		return AppCredentials{}, err
	}
	if !exists {
		return AppCredentials{}, ErrCredentialsNotFound
	}
	fd, err := unix.Openat(int(directory.Fd()), credentialFileName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return AppCredentials{}, ErrCredentialStoreIO
	}
	file := os.NewFile(uintptr(fd), credentialFileName)
	if file == nil {
		unix.Close(fd)
		return AppCredentials{}, ErrCredentialStoreIO
	}
	defer file.Close()
	if err := requirePrivateRegularFile(file); err != nil {
		return AppCredentials{}, err
	}

	payload, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil {
		return AppCredentials{}, ErrCredentialStoreIO
	}
	if len(payload) > maxCredentialFileBytes {
		return AppCredentials{}, ErrStoredCredentialsInvalid
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

func openCredentialDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, ErrCredentialStoreSecurity
		}
		return nil, ErrCredentialStoreIO
	}
	handle := os.NewFile(uintptr(fd), "GitHub App credential directory")
	if handle == nil {
		unix.Close(fd)
		return nil, ErrCredentialStoreIO
	}
	info, err := handle.Stat()
	if err != nil {
		handle.Close()
		return nil, ErrCredentialStoreIO
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		handle.Close()
		return nil, ErrCredentialStoreSecurity
	}
	return handle, nil
}

func inspectCredentialEntry(directoryFD int) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(directoryFD, credentialFileName, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, ErrCredentialStoreIO
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Nlink != 1 {
		return false, ErrCredentialStoreSecurity
	}
	return true, nil
}

func requirePrivateRegularFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return ErrCredentialStoreIO
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return ErrCredentialStoreIO
	}
	// An already-open old generation legitimately reaches link count zero when
	// a concurrent atomic rename replaces it. It remains an immutable private
	// snapshot; reject only additional hard links.
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || stat.Nlink > 1 {
		return ErrCredentialStoreSecurity
	}
	return nil
}

func createCredentialTemporary(directoryFD int) (*os.File, string, error) {
	for range 100 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", ErrCredentialStoreIO
		}
		name := credentialTemporaryStem + hex.EncodeToString(random[:]) + ".tmp"
		fd, err := unix.Openat(directoryFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", ErrCredentialStoreIO
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			unix.Close(fd)
			return nil, "", ErrCredentialStoreIO
		}
		return file, name, nil
	}
	return nil, "", ErrCredentialStoreIO
}
