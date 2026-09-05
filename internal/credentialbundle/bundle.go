// Package credentialbundle implements bounded, age-encrypted credential bundles.
package credentialbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/nebler/fern/internal/strictjson"
)

const (
	Version          = 2
	maxBundleBytes   = 32 << 20
	maxIdentityBytes = 64 << 10
)

var (
	ErrInvalidBundle   = errors.New("invalid credential bundle")
	ErrLegacyBundle    = fmt.Errorf("%w: version 1 is no longer supported; export a new version 2 bundle", ErrInvalidBundle)
	ErrDecryptBundle   = errors.New("credential bundle decryption failed")
	ErrEncryptBundle   = errors.New("credential bundle encryption failed")
	ErrUnsafeFile      = errors.New("unsafe credential bundle file")
	ErrInvalidIdentity = errors.New("invalid age identity")
)

// Binding records the non-secret configuration identity a bundle is bound to.
type Binding struct {
	Workspace      string `json:"workspace"`
	Mode           string `json:"mode"`
	Hostname       string `json:"hostname"`
	AppID          int64  `json:"app_id,omitempty"`
	InstallationID int64  `json:"installation_id,omitempty"`
	RepositoryID   int64  `json:"repository_id"`
	Repository     string `json:"repository"`
}

// Bundle contains serialized candidates. The values are only marshaled inside
// the age stream and callers must keep decrypted values in memory.
type Bundle struct {
	Version   int             `json:"version"`
	Epoch     string          `json:"epoch"`
	CreatedAt time.Time       `json:"created_at"`
	Binding   Binding         `json:"binding"`
	GitHubApp json.RawMessage `json:"github_app,omitempty"`
}

// Fingerprint identifies a decrypted generation without exposing its contents.
func (bundle Bundle) Fingerprint() (string, error) {
	payload, err := marshal(bundle)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (Bundle) String() string          { return "encrypted credential bundle (contents redacted)" }
func (bundle Bundle) GoString() string { return bundle.String() }

// Encrypt buffers the serialized plaintext in memory, then writes it through an
// authenticated age stream. It does not write a plaintext file.
func Encrypt(destination io.Writer, bundle Bundle, recipients []age.Recipient) error {
	if destination == nil || len(recipients) == 0 {
		return ErrEncryptBundle
	}
	payload, err := marshal(bundle)
	if err != nil {
		return err
	}
	writer, err := age.Encrypt(destination, recipients...)
	if err != nil {
		return ErrEncryptBundle
	}
	if _, err := writer.Write(payload); err != nil {
		return ErrEncryptBundle
	}
	if err := writer.Close(); err != nil {
		return ErrEncryptBundle
	}
	return nil
}

// Decrypt authenticates and strictly decodes a bounded bundle in memory.
func Decrypt(source io.Reader, identities []age.Identity) (Bundle, error) {
	if source == nil || len(identities) == 0 {
		return Bundle{}, ErrDecryptBundle
	}
	reader, err := age.Decrypt(io.LimitReader(source, maxBundleBytes+1), identities...)
	if err != nil {
		return Bundle{}, ErrDecryptBundle
	}
	payload, err := io.ReadAll(io.LimitReader(reader, maxBundleBytes+1))
	if err != nil || len(payload) > maxBundleBytes {
		return Bundle{}, ErrDecryptBundle
	}
	if err := strictjson.Check(payload, 64); err != nil {
		return Bundle{}, ErrInvalidBundle
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return Bundle{}, ErrInvalidBundle
	}
	if header.Version == 1 {
		return Bundle{}, ErrLegacyBundle
	}
	var bundle Bundle
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return Bundle{}, ErrInvalidBundle
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return Bundle{}, ErrInvalidBundle
	}
	if err := validate(bundle); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

// ParseRecipients accepts explicit age X25519 recipients.
func ParseRecipients(values []string) ([]age.Recipient, error) {
	result := make([]age.Recipient, 0, len(values))
	for _, value := range values {
		recipient, err := age.ParseX25519Recipient(strings.TrimSpace(value))
		if err != nil {
			return nil, ErrEncryptBundle
		}
		result = append(result, recipient)
	}
	if len(result) == 0 {
		return nil, ErrEncryptBundle
	}
	return result, nil
}

// LoadIdentities reads private X25519 identities from private, regular files.
func LoadIdentities(paths []string) ([]age.Identity, error) {
	var result []age.Identity
	for _, path := range paths {
		file, err := openPrivateFile(path)
		if err != nil {
			return nil, ErrUnsafeFile
		}
		payload, readErr := io.ReadAll(io.LimitReader(file, maxIdentityBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(payload) > maxIdentityBytes {
			return nil, ErrUnsafeFile
		}
		for _, line := range strings.Split(string(payload), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			identity, err := age.ParseX25519Identity(line)
			if err != nil {
				return nil, ErrInvalidIdentity
			}
			result = append(result, identity)
		}
	}
	if len(result) == 0 {
		return nil, ErrInvalidIdentity
	}
	return result, nil
}

// RecipientsForIdentities derives public rollback recipients where supported.
func RecipientsForIdentities(identities []age.Identity) []age.Recipient {
	var result []age.Recipient
	for _, identity := range identities {
		if value, ok := identity.(interface{ Recipient() *age.X25519Recipient }); ok {
			result = append(result, value.Recipient())
		}
	}
	return result
}

// WriteFile installs an encrypted-only mode-0600 artifact with an atomic,
// no-clobber hard link. Parent directories must be trusted by the caller.
func WriteFile(path string, bundle Bundle, recipients []age.Recipient) error {
	if path == "" {
		return ErrUnsafeFile
	}
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafeFile
	}
	temporary, err := os.CreateTemp(directory, ".fern-credentials-*.age")
	if err != nil {
		return ErrUnsafeFile
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return ErrUnsafeFile
	}
	if err := Encrypt(temporary, bundle, recipients); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return ErrUnsafeFile
	}
	if err := temporary.Close(); err != nil {
		return ErrUnsafeFile
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return ErrUnsafeFile
	}
	if err := os.Remove(temporaryPath); err != nil {
		return ErrUnsafeFile
	}
	dir, err := os.Open(directory)
	if err != nil {
		return ErrUnsafeFile
	}
	syncErr, closeErr := dir.Sync(), dir.Close()
	if syncErr != nil || closeErr != nil {
		return ErrUnsafeFile
	}
	return nil
}

// ReadFile decrypts an encrypted artifact without writing plaintext to disk.
func ReadFile(path string, identities []age.Identity) (Bundle, error) {
	file, err := openPrivateFile(path)
	if err != nil {
		return Bundle{}, ErrUnsafeFile
	}
	bundle, decryptErr := Decrypt(file, identities)
	closeErr := file.Close()
	if decryptErr != nil {
		return Bundle{}, decryptErr
	}
	if closeErr != nil {
		return Bundle{}, ErrUnsafeFile
	}
	return bundle, nil
}

func marshal(bundle Bundle) ([]byte, error) {
	if err := validate(bundle); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(bundle)
	if err != nil || len(payload) > maxBundleBytes || strictjson.Check(payload, 64) != nil {
		return nil, ErrInvalidBundle
	}
	return append(payload, '\n'), nil
}

func validate(bundle Bundle) error {
	if bundle.Version == 1 {
		return ErrLegacyBundle
	}
	validAtom := func(value string) bool {
		return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
	}
	if bundle.Version != Version || !validAtom(bundle.Epoch) || strings.ContainsAny(bundle.Epoch, " /\\\t\r\n") || bundle.CreatedAt.IsZero() || bundle.CreatedAt.Location() != time.UTC ||
		!validAtom(bundle.Binding.Workspace) || bundle.Binding.Mode != "github-app-broker" || !validAtom(bundle.Binding.Hostname) ||
		bundle.Binding.RepositoryID <= 0 || !validAtom(bundle.Binding.Repository) ||
		len(bundle.GitHubApp) == 0 || bytes.Equal(bytes.TrimSpace(bundle.GitHubApp), []byte("null")) || len(bundle.GitHubApp) > 256<<10 {
		return ErrInvalidBundle
	}
	if bundle.Binding.AppID <= 0 || bundle.Binding.InstallationID <= 0 {
		return ErrInvalidBundle
	}
	return nil
}

// Summary returns non-secret operator output.
func Summary(bundle Bundle) string {
	fingerprint, err := bundle.Fingerprint()
	if err != nil {
		return "credential bundle (invalid)"
	}
	return fmt.Sprintf("epoch=%s fingerprint=sha256:%s", bundle.Epoch, fingerprint)
}
