package taskartifact

import (
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/task"
)

const (
	MaxCommandTimeout = 2 * time.Minute
	MaxOutputBytes    = 64 << 20
	MaxBundleBytes    = 512 << 20
	MaxManifestFiles  = 100_000
	MaxBlobBytes      = int64(2 << 30)

	defaultTimeout     = 15 * time.Second
	defaultOutputBytes = 16 << 20
	defaultBundleBytes = 64 << 20
	defaultFiles       = 10_000
	defaultBlobBytes   = int64(64 << 20)
)

var (
	ErrInvalidConfig  = errors.New("invalid task artifact engine configuration")
	ErrInvalidSource  = errors.New("invalid task artifact source")
	ErrInvalidSpec    = errors.New("invalid task artifact snapshot specification")
	ErrUnsafeSource   = errors.New("unsafe task artifact source repository")
	ErrGitFailed      = errors.New("task artifact Git command failed")
	ErrGitTimeout     = errors.New("task artifact Git command timed out")
	ErrOutputLimit    = errors.New("task artifact output limit exceeded")
	ErrVerification   = errors.New("task artifact verification failed")
	ErrInvalidLocator = errors.New("invalid task artifact locator")
	ErrStorage        = errors.New("task artifact storage integrity failure")
	ErrCheckout       = errors.New("task artifact checkout integrity failure")
)

// Config is trusted host policy. All paths must be exact absolute paths to
// existing, non-symlink filesystem objects. CASRoot and WorkRoot must be
// distinct private directories owned by the current user.
type Config struct {
	GitExecutable  string
	CASRoot        string
	WorkRoot       string
	CommandTimeout time.Duration
	OutputBytes    int
	BundleBytes    int64
	ManifestFiles  int
	BlobBytes      int64
}

// Source identifies the exact provider-selected clone and background-run
// attempt. Its path is intentionally available only through construction and
// engine methods.
type Source struct {
	WorkspaceID task.WorkspaceID
	TaskID      task.TaskID
	AttemptID   task.AttemptID
	path        string
}

func NewSource(path string, workspaceID task.WorkspaceID, taskID task.TaskID, attemptID task.AttemptID) (Source, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Source{}, fmt.Errorf("%w: repository path", ErrInvalidSource)
	}
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return Source{}, fmt.Errorf("%w: workspace ID", ErrInvalidSource)
	}
	if _, err := task.ParseTaskID(string(taskID)); err != nil {
		return Source{}, fmt.Errorf("%w: task ID", ErrInvalidSource)
	}
	if _, err := task.ParseAttemptID(string(attemptID)); err != nil {
		return Source{}, fmt.Errorf("%w: attempt ID", ErrInvalidSource)
	}
	return Source{WorkspaceID: workspaceID, TaskID: taskID, AttemptID: attemptID, path: path}, nil
}

// SnapshotSpec binds a snapshot to an admitted base and a caller-persisted UTC
// epoch second. The epoch is the sole variable input to the normalized commit.
type SnapshotSpec struct {
	Source      Source
	Base        task.GitOID
	EpochSecond int64
}

// FileVersion is the exact Git representation of one side of a change.
type FileVersion struct {
	Mode    string      `json:"mode"`
	BlobOID task.GitOID `json:"blob_oid"`
	Size    int64       `json:"size"`
}

// ChangeEntry records a raw-byte path as standard padded Base64. Entries are
// sorted by decoded path bytes, not by their Base64 spelling.
type ChangeEntry struct {
	PathBase64 string       `json:"path_base64"`
	Kind       string       `json:"kind"`
	Old        *FileVersion `json:"old,omitempty"`
	New        *FileVersion `json:"new,omitempty"`
}

// Digest is an opaque SHA-256 value.
type Digest struct{ value [32]byte }

func ParseDigest(value string) (Digest, error) {
	var digest Digest
	if len(value) != 64 || value != strings.ToLower(value) {
		return digest, fmt.Errorf("%w: SHA-256", ErrInvalidLocator)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return digest, fmt.Errorf("%w: SHA-256", ErrInvalidLocator)
	}
	copy(digest.value[:], decoded)
	return digest, nil
}

func (d Digest) String() string { return hex.EncodeToString(d.value[:]) }

// Snapshot is the verified, normalized content description. Slices returned
// by Engine methods do not alias engine-owned state.
type Snapshot struct {
	Base           task.GitOID
	Result         task.GitOID
	Tree           task.GitOID
	EpochSecond    int64
	Changes        []ChangeEntry
	ManifestSHA256 Digest
	BundleSHA256   Digest
	BundleBytes    int64
}

// StagedLocator is an engine-issued capability for a verified staged artifact.
// It is intentionally not serializable and is invalid after Store succeeds.
type StagedLocator struct {
	engine *Engine
	path   string
	device uint64
	inode  uint64
	digest Digest
}

// Locator is the only persistable CAS authority accepted by Inspect and
// Materialize. ParseLocator is the external-data validation boundary.
type Locator struct {
	digest Digest
	valid  bool
}

func ParseLocator(value string) (Locator, error) {
	algorithm, encoded, ok := strings.Cut(value, ":")
	if !ok || algorithm != "sha256" {
		return Locator{}, ErrInvalidLocator
	}
	digest, err := ParseDigest(encoded)
	if err != nil {
		return Locator{}, err
	}
	return Locator{digest: digest, valid: true}, nil
}

func (l Locator) String() string {
	if !l.valid {
		return ""
	}
	return "sha256:" + l.digest.String()
}
func (l Locator) Digest() Digest { return l.digest }

func (l Locator) MarshalText() ([]byte, error) {
	if !l.valid {
		return nil, ErrInvalidLocator
	}
	return []byte(l.String()), nil
}

func (l *Locator) UnmarshalText(value []byte) error {
	if l == nil {
		return ErrInvalidLocator
	}
	parsed, err := ParseLocator(string(value))
	if err != nil {
		return err
	}
	*l = parsed
	return nil
}

// Checkout is an engine-owned detached checkout. Close removes it only after
// revalidating its marker and filesystem identity.
type Checkout struct {
	engine       *Engine
	path         string
	marker       string
	device       uint64
	inode        uint64
	markerDevice uint64
	markerInode  uint64
	mu           sync.Mutex
	closed       bool
}

func (c *Checkout) Path() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ""
	}
	return c.path
}
