package taskresult

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	MaxTimeout       = time.Minute
	MaxOutputBytes   = 64 << 20
	MaxEvidenceBytes = 16 << 10
	MaxManifestFiles = 10000
	defaultTimeout   = 15 * time.Second
	defaultOutput    = 64 << 20
)

var (
	ErrInvalidConfig   = errors.New("invalid task result collector configuration")
	ErrInvalidRequest  = errors.New("invalid task result collection request")
	ErrRepositoryProof = errors.New("repository result proof failed")
	ErrGitTimeout      = errors.New("result Git command timed out")
	ErrGitOutputLimit  = errors.New("result Git command exceeded output limit")
)

// Config contains trusted host policy. GitExecutable must be an absolute
// executable path; no shell or ambient environment is used.
type Config struct {
	GitExecutable string
	Timeout       time.Duration
	OutputBytes   int
	Now           func() time.Time
}

// Request binds collection to the persisted task and exact OpenCode success
// identity. EvidencePayload must already be sanitized by the coordinator.
type Request struct {
	RepositoryPath    string
	Repository        task.RepositoryTuple
	OpenCodeSessionID task.OpenCodeSessionID
	OpenCodeMessageID task.OpenCodeMessageID
	EvidencePayload   json.RawMessage
	EvidenceSHA256    [32]byte
	PolicyVersion     string
}

// Result contains exactly the values needed to construct the Git/evidence
// portion of taskstore.SealResultParams. Reference-backed values are fresh
// copies and may be modified by the caller.
type Result struct {
	Tuple             task.ResultTuple
	TreeOID           task.GitOID
	Manifest          []taskstore.ManifestEntry
	ManifestSHA256    [32]byte
	OpenCodeSessionID task.OpenCodeSessionID
	OpenCodeMessageID task.OpenCodeMessageID
	EvidencePayload   json.RawMessage
	EvidenceSHA256    [32]byte
	PolicyVersion     string
	CollectedAt       time.Time
}

// Collector is immutable after construction and safe for concurrent use.
type Collector struct {
	gitExecutable string
	gitFile       os.FileInfo
	timeout       time.Duration
	outputBytes   int
	now           func() time.Time
}

func New(config Config) (*Collector, error) {
	if !filepath.IsAbs(config.GitExecutable) || filepath.Clean(config.GitExecutable) != config.GitExecutable {
		return nil, fmt.Errorf("%w: Git executable path", ErrInvalidConfig)
	}
	info, err := os.Lstat(config.GitExecutable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("%w: Git executable", ErrInvalidConfig)
	}
	if config.Timeout == 0 {
		config.Timeout = defaultTimeout
	}
	if config.OutputBytes == 0 {
		config.OutputBytes = defaultOutput
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Timeout <= 0 || config.Timeout > MaxTimeout || config.OutputBytes <= 0 || config.OutputBytes > MaxOutputBytes {
		return nil, fmt.Errorf("%w: timeout or output bound", ErrInvalidConfig)
	}
	return &Collector{gitExecutable: config.GitExecutable, gitFile: info, timeout: config.Timeout, outputBytes: config.OutputBytes, now: config.Now}, nil
}

// Collect performs read-only proof and collection. The caller must hold the
// workspace.Manager AcquireQuiesced fence for the full call and through the
// subsequent SealResult transaction.
func (c *Collector) Collect(ctx context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	repositoryPath, err := secureRepositoryPath(request.RepositoryPath)
	if err != nil {
		return Result{}, err
	}
	collectContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if err := c.proveStaticSafety(collectContext, repositoryPath); err != nil {
		return Result{}, err
	}
	resultCommit, treeOID, err := c.proveState(collectContext, repositoryPath, request.Repository.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	manifest, digest, err := c.buildManifest(collectContext, repositoryPath, request.Repository.BaseSHA, resultCommit)
	if err != nil {
		return Result{}, err
	}
	outcome := task.ResultChanged
	if resultCommit == request.Repository.BaseSHA {
		outcome = task.ResultNoChanges
	}
	tuple := task.ResultTuple{RepositoryTuple: request.Repository, ResultCommit: resultCommit, Outcome: outcome, ManifestEntries: len(manifest), WorktreeClean: true}
	if err := tuple.ValidateAgainst(request.Repository); err != nil {
		return Result{}, fmt.Errorf("%w: unrepresentable result", ErrRepositoryProof)
	}

	// Recompute object-derived data and repeat all mutable checkout proofs. This
	// is deliberately the last work before constructing the return value.
	if err := c.proveStaticSafety(collectContext, repositoryPath); err != nil {
		return Result{}, err
	}
	finalCommit, finalTree, err := c.proveState(collectContext, repositoryPath, request.Repository.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	if finalCommit != resultCommit || finalTree != treeOID {
		return Result{}, fmt.Errorf("%w: repository changed during collection", ErrRepositoryProof)
	}
	finalManifest, finalDigest, err := c.buildManifest(collectContext, repositoryPath, request.Repository.BaseSHA, finalCommit)
	if err != nil {
		return Result{}, err
	}
	if finalDigest != digest || !manifestsEqual(manifest, finalManifest) {
		return Result{}, fmt.Errorf("%w: objects changed during collection", ErrRepositoryProof)
	}
	if err := c.proveHeadAndClean(collectContext, repositoryPath, resultCommit); err != nil {
		return Result{}, err
	}

	collectedAt := c.now().UTC().Truncate(time.Millisecond)
	if collectedAt.IsZero() || collectedAt.UnixMilli() < 0 {
		return Result{}, fmt.Errorf("%w: time source", ErrInvalidConfig)
	}
	return Result{
		Tuple: tuple, TreeOID: treeOID, Manifest: cloneManifest(manifest), ManifestSHA256: digest,
		OpenCodeSessionID: request.OpenCodeSessionID, OpenCodeMessageID: request.OpenCodeMessageID,
		EvidencePayload: append(json.RawMessage(nil), request.EvidencePayload...), EvidenceSHA256: request.EvidenceSHA256,
		PolicyVersion: request.PolicyVersion, CollectedAt: collectedAt,
	}, nil
}

func validateRequest(request Request) error {
	if !filepath.IsAbs(request.RepositoryPath) || filepath.Clean(request.RepositoryPath) != request.RepositoryPath {
		return fmt.Errorf("%w: repository path", ErrInvalidRequest)
	}
	if err := request.Repository.Validate(); err != nil {
		return fmt.Errorf("%w: repository tuple", ErrInvalidRequest)
	}
	if _, err := task.ParseOpenCodeSessionID(string(request.OpenCodeSessionID)); err != nil {
		return fmt.Errorf("%w: OpenCode session ID", ErrInvalidRequest)
	}
	if _, err := task.ParseOpenCodeMessageID(string(request.OpenCodeMessageID)); err != nil {
		return fmt.Errorf("%w: OpenCode message ID", ErrInvalidRequest)
	}
	if !validPolicyVersion(request.PolicyVersion) {
		return fmt.Errorf("%w: policy version", ErrInvalidRequest)
	}
	if err := validateEvidence(request.EvidencePayload, request.EvidenceSHA256); err != nil {
		return err
	}
	return nil
}

func validPolicyVersion(value string) bool {
	if !utf8.ValidString(value) || len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validateEvidence(payload json.RawMessage, expected [32]byte) error {
	trimmed := bytes.TrimSpace(payload)
	if len(payload) < 2 || len(payload) > MaxEvidenceBytes || len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !json.Valid(payload) {
		return fmt.Errorf("%w: evidence must be a bounded JSON object", ErrInvalidRequest)
	}
	if sha256.Sum256(payload) != expected {
		return fmt.Errorf("%w: evidence digest", ErrInvalidRequest)
	}
	var decoded any
	if json.Unmarshal(payload, &decoded) != nil || containsSensitiveEvidenceKey(decoded) {
		return fmt.Errorf("%w: sensitive evidence", ErrInvalidRequest)
	}
	return nil
}

func containsSensitiveEvidenceKey(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			switch normalized {
			case "prompt", "rawprompt", "credential", "credentials", "authorization", "token", "cookie", "setcookie", "body", "rawbody", "requestbody", "responsebody":
				return true
			}
			if containsSensitiveEvidenceKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsSensitiveEvidenceKey(child) {
				return true
			}
		}
	}
	return false
}

func secureRepositoryPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: repository is not a real directory", ErrInvalidRequest)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil || realPath != path {
		return "", fmt.Errorf("%w: repository path contains a symlink", ErrInvalidRequest)
	}
	gitDirectory := filepath.Join(path, ".git")
	gitInfo, err := os.Lstat(gitDirectory)
	if err != nil || !gitInfo.IsDir() || gitInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: .git is not a real directory", ErrInvalidRequest)
	}
	realGitDirectory, err := filepath.EvalSymlinks(gitDirectory)
	if err != nil || realGitDirectory != gitDirectory {
		return "", fmt.Errorf("%w: .git path contains a symlink", ErrInvalidRequest)
	}
	return path, nil
}

func (c *Collector) buildManifest(ctx context.Context, repositoryPath string, base, result task.GitOID) ([]taskstore.ManifestEntry, [32]byte, error) {
	if base == result {
		digest := sha256.Sum256([]byte("[]"))
		return []taskstore.ManifestEntry{}, digest, nil
	}
	output, err := c.git(ctx, repositoryPath, c.outputBytes, nil, "diff-tree", "--no-commit-id", "--raw", "-r", "-z", "--abbrev=40", "--no-renames", "--no-ext-diff", string(base), string(result), "--")
	if err != nil {
		return nil, [32]byte{}, err
	}
	changes, err := parseRawDiff(output)
	if err != nil || len(changes) == 0 || len(changes) > MaxManifestFiles {
		return nil, [32]byte{}, fmt.Errorf("%w: invalid manifest diff", ErrRepositoryProof)
	}
	sort.Slice(changes, func(i, j int) bool { return bytes.Compare(changes[i].path, changes[j].path) < 0 })
	for index := 1; index < len(changes); index++ {
		if bytes.Equal(changes[index-1].path, changes[index].path) {
			return nil, [32]byte{}, fmt.Errorf("%w: duplicate manifest path", ErrRepositoryProof)
		}
	}
	sizes, err := c.blobSizes(ctx, repositoryPath, changes)
	if err != nil {
		return nil, [32]byte{}, err
	}
	manifest := make([]taskstore.ManifestEntry, 0, len(changes))
	for _, change := range changes {
		entry := taskstore.ManifestEntry{PathBase64: base64.StdEncoding.EncodeToString(change.path), ChangeKind: change.kind}
		if change.oldOID != "" {
			mode, oid, size := change.oldMode, string(change.oldOID), sizes[change.oldOID]
			entry.OldMode, entry.OldBlobOID, entry.OldSize = &mode, &oid, &size
		}
		if change.newOID != "" {
			mode, oid, size := change.newMode, string(change.newOID), sizes[change.newOID]
			entry.NewMode, entry.NewBlobOID, entry.NewSize = &mode, &oid, &size
		}
		manifest = append(manifest, entry)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("%w: encode manifest", ErrRepositoryProof)
	}
	return manifest, sha256.Sum256(encoded), nil
}

func manifestsEqual(left, right []taskstore.ManifestEntry) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func cloneManifest(input []taskstore.ManifestEntry) []taskstore.ManifestEntry {
	result := make([]taskstore.ManifestEntry, len(input))
	for index, entry := range input {
		result[index] = entry
		if entry.OldMode != nil {
			value := *entry.OldMode
			result[index].OldMode = &value
		}
		if entry.NewMode != nil {
			value := *entry.NewMode
			result[index].NewMode = &value
		}
		if entry.OldBlobOID != nil {
			value := *entry.OldBlobOID
			result[index].OldBlobOID = &value
		}
		if entry.NewBlobOID != nil {
			value := *entry.NewBlobOID
			result[index].NewBlobOID = &value
		}
		if entry.OldSize != nil {
			value := *entry.OldSize
			result[index].OldSize = &value
		}
		if entry.NewSize != nil {
			value := *entry.NewSize
			result[index].NewSize = &value
		}
	}
	return result
}
