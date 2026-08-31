package taskartifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/task"
)

const (
	manifestName = "manifest.json"
	bundleName   = "result.bundle"
	markerName   = ".fern-artifact-checkout"
)

// Engine is immutable after construction and safe for concurrent use.
type Engine struct {
	gitExecutable string
	gitFile       os.FileInfo
	casRoot       string
	workRoot      string
	timeout       time.Duration
	outputBytes   int
	bundleBytes   int64
	manifestFiles int
	blobBytes     int64
	mu            sync.Mutex
	checkouts     map[*Checkout]struct{}
	closed        bool
}

func New(config Config) (*Engine, error) {
	if config.CommandTimeout == 0 {
		config.CommandTimeout = defaultTimeout
	}
	if config.OutputBytes == 0 {
		config.OutputBytes = defaultOutputBytes
	}
	if config.BundleBytes == 0 {
		config.BundleBytes = defaultBundleBytes
	}
	if config.ManifestFiles == 0 {
		config.ManifestFiles = defaultFiles
	}
	if config.BlobBytes == 0 {
		config.BlobBytes = defaultBlobBytes
	}
	if config.CommandTimeout <= 0 || config.CommandTimeout > MaxCommandTimeout || config.OutputBytes <= 0 || config.OutputBytes > MaxOutputBytes ||
		config.BundleBytes <= 0 || config.BundleBytes > MaxBundleBytes || config.ManifestFiles <= 0 || config.ManifestFiles > MaxManifestFiles ||
		config.BlobBytes <= 0 || config.BlobBytes > MaxBlobBytes {
		return nil, fmt.Errorf("%w: bounds", ErrInvalidConfig)
	}
	gitInfo, err := exactRegular(config.GitExecutable, true)
	if err != nil || gitInfo.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: Git executable", ErrInvalidConfig)
	}
	if err := privateRoot(config.CASRoot); err != nil {
		return nil, fmt.Errorf("%w: CAS root: %v", ErrInvalidConfig, err)
	}
	if err := privateRoot(config.WorkRoot); err != nil {
		return nil, fmt.Errorf("%w: work root: %v", ErrInvalidConfig, err)
	}
	if config.CASRoot == config.WorkRoot || pathContains(config.CASRoot, config.WorkRoot) || pathContains(config.WorkRoot, config.CASRoot) {
		return nil, fmt.Errorf("%w: roots must be disjoint", ErrInvalidConfig)
	}
	if err := removeInterruptedDirectories(config.CASRoot, ".stage-", ".remove-"); err != nil {
		return nil, fmt.Errorf("%w: reconcile CAS temporary state", ErrInvalidConfig)
	}
	if err := removeInterruptedDirectories(config.WorkRoot, ".verify-", "checkout-", ".remove-"); err != nil {
		return nil, fmt.Errorf("%w: reconcile work temporary state", ErrInvalidConfig)
	}
	return &Engine{gitExecutable: config.GitExecutable, gitFile: gitInfo, casRoot: config.CASRoot, workRoot: config.WorkRoot,
		timeout: config.CommandTimeout, outputBytes: config.OutputBytes, bundleBytes: config.BundleBytes,
		manifestFiles: config.ManifestFiles, blobBytes: config.BlobBytes, checkouts: make(map[*Checkout]struct{})}, nil
}

func removeInterruptedDirectories(root string, prefixes ...string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		matched := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry.Name(), prefix) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !safeDirectoryInfo(info) {
			return ErrStorage
		}
		device, inode, err := fileIdentity(info)
		if err != nil {
			return err
		}
		if err := removeExactDirectory(path, device, inode); err != nil {
			return err
		}
	}
	return syncDirectory(root)
}

// Close removes all still-live engine checkouts after coordinators have
// stopped. CAS objects are immutable and remain installed.
func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	e.closed = true
	values := make([]*Checkout, 0, len(e.checkouts))
	for checkout := range e.checkouts {
		values = append(values, checkout)
	}
	e.mu.Unlock()
	var result error
	for _, checkout := range values {
		result = errors.Join(result, checkout.Close())
	}
	return result
}

// Snapshot captures the final nonignored worktree state without changing the
// source HEAD, index, or worktree. It returns an independently verified staged
// capability that must be installed with Store.
func (e *Engine) Snapshot(ctx context.Context, spec SnapshotSpec) (Snapshot, StagedLocator, error) {
	if err := validateSpec(spec); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	stage, err := e.makeTemp(e.casRoot, ".stage-")
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	stageDevice, stageInode, err := directoryIdentity(stage)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = removeExactDirectory(stage, stageDevice, stageInode)
		}
	}()
	identity, err := e.admitSource(ctx, spec.Source.path, spec.Base)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	treeOne, err := e.captureTree(ctx, spec.Source.path, stage, "index-one")
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	treeTwo, err := e.captureTree(ctx, spec.Source.path, stage, "index-two")
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if treeOne != treeTwo {
		return Snapshot{}, StagedLocator{}, fmt.Errorf("%w: unstable worktree", ErrUnsafeSource)
	}
	defer os.Remove(filepath.Join(stage, "index-one"))
	defer os.Remove(filepath.Join(stage, "index-two"))
	if err := e.proveTree(ctx, spec.Source.path, treeOne); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if err := e.checkSourceIdentity(ctx, spec.Source.path, spec.Base, identity); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	baseTree, err := e.oid(ctx, spec.Source.path, string(spec.Base)+"^{tree}", nil)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	result := spec.Base
	if treeOne != baseTree {
		result, err = e.commitTree(ctx, spec.Source.path, treeOne, spec.Base, spec.EpochSecond)
		if err != nil {
			return Snapshot{}, StagedLocator{}, err
		}
	}
	changes, err := e.buildChanges(ctx, spec.Source.path, spec.Base, result)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if (result == spec.Base) != (len(changes) == 0) {
		return Snapshot{}, StagedLocator{}, fmt.Errorf("%w: inconsistent no-change result", ErrVerification)
	}
	_, changesDigest, err := canonicalChanges(changes)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	bundlePath := filepath.Join(stage, bundleName)
	bundleDigest, bundleSize, err := e.createBundle(ctx, spec.Source.path, bundlePath, spec.Base, result)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	manifest := artifactManifest{
		Version: 2, RepositoryID: spec.RepositoryID, WorkspaceID: spec.Source.WorkspaceID, TaskID: spec.Source.TaskID,
		AttemptID: spec.Source.AttemptID, Generation: spec.Generation, SealRequestID: spec.SealRequestID,
		ImageIdentity: spec.ImageIdentity, Profile: spec.Profile, ProfileSHA256: spec.ProfileSHA256,
		EnvironmentSHA256: spec.EnvironmentSHA256, ResourceSpecVersion: spec.ResourceSpecVersion,
		OpenCodeSessionID: spec.OpenCodeSessionID, OpenCodeMessageID: spec.OpenCodeMessageID,
		SnapshotPolicyVersion: spec.SnapshotPolicyVersion, CompletionAuthority: CompletionUserSeal,
		Base: spec.Base, Result: result, Tree: treeOne, EpochSecond: spec.EpochSecond,
		Changes: changes, ChangesSHA256: changesDigest, BundleSHA256: bundleDigest, BundleBytes: bundleSize,
	}
	manifestBytes, manifestDigest, err := encodeManifest(manifest)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if len(manifestBytes) > e.outputBytes {
		return Snapshot{}, StagedLocator{}, ErrOutputLimit
	}
	if err := writePrivateFile(filepath.Join(stage, manifestName), manifestBytes); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if err := syncDirectory(stage); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if _, err := e.verifyArtifact(ctx, manifestBytes, bundlePath, 0o600, manifestDigest); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	finalTree, err := e.refreshCapturedTree(ctx, spec.Source.path, filepath.Join(stage, "index-two"))
	if err != nil || finalTree != treeOne {
		return Snapshot{}, StagedLocator{}, fmt.Errorf("%w: worktree changed during snapshot", ErrUnsafeSource)
	}
	if err := e.checkSourceIdentity(ctx, spec.Source.path, spec.Base, identity); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if err := os.Remove(filepath.Join(stage, "index-one")); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if err := os.Remove(filepath.Join(stage, "index-two")); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	info, err := os.Lstat(stage)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	device, inode, err := fileIdentity(info)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	keep = true
	snapshot := snapshotFromManifest(manifest, manifestDigest)
	return snapshot, StagedLocator{engine: e, path: stage, device: device, inode: inode, digest: manifestDigest}, nil
}

func validateSpec(spec SnapshotSpec) error {
	if _, err := task.ParseWorkspaceID(string(spec.Source.WorkspaceID)); err != nil ||
		spec.Source.path == "" {
		return fmt.Errorf("%w: source", ErrInvalidSpec)
	}
	if _, err := task.ParseTaskID(string(spec.Source.TaskID)); err != nil {
		return fmt.Errorf("%w: source", ErrInvalidSpec)
	}
	if _, err := task.ParseAttemptID(string(spec.Source.AttemptID)); err != nil {
		return fmt.Errorf("%w: source", ErrInvalidSpec)
	}
	if _, err := task.ParseRepositoryID(strconv.FormatUint(uint64(spec.RepositoryID), 10)); err != nil {
		return fmt.Errorf("%w: repository ID", ErrInvalidSpec)
	}
	if spec.Generation <= 0 || spec.Generation > maxGeneration {
		return fmt.Errorf("%w: generation", ErrInvalidSpec)
	}
	if _, err := task.ParseSealRequestID(string(spec.SealRequestID)); err != nil {
		return fmt.Errorf("%w: seal request ID", ErrInvalidSpec)
	}
	if !validImageIdentity(spec.ImageIdentity) || !validProfile(spec.Profile) || !validDigest(spec.ProfileSHA256) || !validDigest(spec.EnvironmentSHA256) {
		return fmt.Errorf("%w: execution identity", ErrInvalidSpec)
	}
	if spec.ResourceSpecVersion != ResourceSpecVersion || spec.SnapshotPolicyVersion != SnapshotPolicyV1 {
		return fmt.Errorf("%w: policy identity", ErrInvalidSpec)
	}
	if _, err := task.ParseOpenCodeSessionID(string(spec.OpenCodeSessionID)); err != nil {
		return fmt.Errorf("%w: OpenCode session ID", ErrInvalidSpec)
	}
	if _, err := task.ParseOpenCodeMessageID(string(spec.OpenCodeMessageID)); err != nil {
		return fmt.Errorf("%w: OpenCode message ID", ErrInvalidSpec)
	}
	if _, err := task.ParseGitOID(string(spec.Base)); err != nil || spec.EpochSecond < 0 || spec.EpochSecond > 253402300799 {
		return fmt.Errorf("%w: base or epoch", ErrInvalidSpec)
	}
	return nil
}

func (e *Engine) makeTemp(root, prefix string) (string, error) {
	if err := privateRoot(root); err != nil {
		return "", fmt.Errorf("%w: root changed", ErrStorage)
	}
	path, err := os.MkdirTemp(root, prefix)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func randomToken() (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value[:]), nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func cloneChanges(entries []ChangeEntry) []ChangeEntry {
	result := make([]ChangeEntry, len(entries))
	for index, entry := range entries {
		result[index] = entry
		if entry.Old != nil {
			copy := *entry.Old
			result[index].Old = &copy
		}
		if entry.New != nil {
			copy := *entry.New
			result[index].New = &copy
		}
	}
	return result
}

func snapshotFromManifest(manifest artifactManifest, digest Digest) Snapshot {
	return Snapshot{
		RepositoryID: manifest.RepositoryID, WorkspaceID: manifest.WorkspaceID, TaskID: manifest.TaskID, AttemptID: manifest.AttemptID,
		Generation: manifest.Generation, SealRequestID: manifest.SealRequestID, ImageIdentity: manifest.ImageIdentity,
		Profile: manifest.Profile, ProfileSHA256: manifest.ProfileSHA256, EnvironmentSHA256: manifest.EnvironmentSHA256,
		ResourceSpecVersion: manifest.ResourceSpecVersion, OpenCodeSessionID: manifest.OpenCodeSessionID,
		OpenCodeMessageID: manifest.OpenCodeMessageID, SnapshotPolicyVersion: manifest.SnapshotPolicyVersion,
		CompletionAuthority: manifest.CompletionAuthority, Base: manifest.Base, Result: manifest.Result, Tree: manifest.Tree,
		EpochSecond: manifest.EpochSecond, Changes: cloneChanges(manifest.Changes), ChangesSHA256: manifest.ChangesSHA256,
		ManifestSHA256: digest, BundleSHA256: manifest.BundleSHA256, BundleBytes: manifest.BundleBytes,
	}
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if len(value) > b.limit-b.Len() {
		remaining := b.limit - b.Len()
		if remaining > 0 {
			_, _ = b.Buffer.Write(value[:remaining])
		}
		b.over = true
		return len(value), errLimit
	}
	return b.Buffer.Write(value)
}

var errLimit = errors.New("bounded writer limit")

type boundedHashWriter struct {
	file  *os.File
	hash  hash.Hash
	limit int64
	size  int64
}

func (w *boundedHashWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.limit-w.size {
		return 0, errLimit
	}
	n, err := w.file.Write(value)
	if n > 0 {
		_, _ = w.hash.Write(value[:n])
		w.size += int64(n)
	}
	return n, err
}

func sha256Bytes(value []byte) Digest { return Digest{value: sha256.Sum256(value)} }
