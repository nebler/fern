package taskartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/atomicfile"
	"github.com/nebler/fern/internal/domain"
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
		device, inode, err := atomicfile.Identity(info)
		if err != nil {
			return err
		}
		if err := removeExactDirectory(path, device, inode); err != nil {
			return err
		}
	}
	return atomicfile.SyncDir(root)
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
	device, inode, err := directoryIdentity(stage)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = removeExactDirectory(stage, device, inode)
		}
	}()
	identity, tree, err := e.captureStableTree(ctx, spec, stage)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	manifest, manifestBytes, digest, err := e.stageArtifact(ctx, spec, stage, tree)
	if err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if _, err := e.verifyArtifact(ctx, manifestBytes, filepath.Join(stage, bundleName), 0o600, digest); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	if err := e.confirmSourceUnchanged(ctx, spec, stage, tree, identity); err != nil {
		return Snapshot{}, StagedLocator{}, err
	}
	keep = true
	return snapshotFromManifest(manifest, digest), StagedLocator{engine: e, path: stage, device: device, inode: inode, digest: digest}, nil
}

// captureStableTree admits the source and captures its worktree as a tree
// twice, into private indexes in stage, requiring both captures to agree.
func (e *Engine) captureStableTree(ctx context.Context, spec SnapshotSpec, stage string) (sourceIdentity, domain.GitOID, error) {
	source := spec.Source.path
	identity, err := e.admitSource(ctx, source, spec.Base)
	if err != nil {
		return sourceIdentity{}, "", err
	}
	treeOne, err := e.captureTree(ctx, source, stage, "index-one")
	if err != nil {
		return sourceIdentity{}, "", err
	}
	treeTwo, err := e.captureTree(ctx, source, stage, "index-two")
	if err != nil {
		return sourceIdentity{}, "", err
	}
	if treeOne != treeTwo {
		return sourceIdentity{}, "", fmt.Errorf("%w: unstable worktree", ErrUnsafeSource)
	}
	if err := e.proveTree(ctx, source, treeOne); err != nil {
		return sourceIdentity{}, "", err
	}
	if err := e.checkSourceIdentity(ctx, source, spec.Base, identity); err != nil {
		return sourceIdentity{}, "", err
	}
	return identity, treeOne, nil
}

// stageArtifact commits tree onto the base (unless unchanged), writes the
// bundle and canonical manifest into stage, and returns the manifest.
func (e *Engine) stageArtifact(ctx context.Context, spec SnapshotSpec, stage string, tree domain.GitOID) (artifactManifest, []byte, Digest, error) {
	source := spec.Source.path
	baseTree, err := e.oid(ctx, source, string(spec.Base)+"^{tree}", nil)
	if err != nil {
		return artifactManifest{}, nil, Digest{}, err
	}
	result := spec.Base
	if tree != baseTree {
		result, err = e.commitTree(ctx, source, tree, spec.Base, spec.EpochSecond)
		if err != nil {
			return artifactManifest{}, nil, Digest{}, err
		}
	}
	changes, err := e.buildChanges(ctx, source, spec.Base, result)
	if err != nil {
		return artifactManifest{}, nil, Digest{}, err
	}
	if (result == spec.Base) != (len(changes) == 0) {
		return artifactManifest{}, nil, Digest{}, fmt.Errorf("%w: inconsistent no-change result", ErrVerification)
	}
	_, changesDigest, err := canonicalChanges(changes)
	if err != nil {
		return artifactManifest{}, nil, Digest{}, err
	}
	bundleDigest, bundleSize, err := e.createBundle(ctx, source, filepath.Join(stage, bundleName), spec.Base, result)
	if err != nil {
		return artifactManifest{}, nil, Digest{}, err
	}
	manifest := artifactManifest{
		Version: 3, RepositoryID: spec.RepositoryID, WorkspaceID: spec.Source.WorkspaceID, RunID: spec.Source.RunID,
		ResultID:      spec.ResultID,
		ImageIdentity: spec.ImageIdentity, Profile: spec.Profile, ProfileSHA256: spec.ProfileSHA256,
		EnvironmentSHA256: spec.EnvironmentSHA256, ResourceSpecVersion: spec.ResourceSpecVersion,
		OpenCodeSessionID: spec.OpenCodeSessionID, OpenCodeMessageID: spec.OpenCodeMessageID,
		SnapshotPolicyVersion: spec.SnapshotPolicyVersion, CompletionAuthority: CompletionUserSeal,
		Base: spec.Base, Result: result, Tree: tree, EpochSecond: spec.EpochSecond,
		Changes: changes, ChangesSHA256: changesDigest, BundleSHA256: bundleDigest, BundleBytes: bundleSize,
	}
	manifestBytes, digest, err := encodeManifest(manifest)
	if err != nil {
		return artifactManifest{}, nil, Digest{}, err
	}
	if len(manifestBytes) > e.outputBytes {
		return artifactManifest{}, nil, Digest{}, ErrOutputLimit
	}
	if err := atomicfile.WriteExclusive(filepath.Join(stage, manifestName), manifestBytes, 0o600); err != nil {
		return artifactManifest{}, nil, Digest{}, err
	}
	return manifest, manifestBytes, digest, nil
}

// confirmSourceUnchanged recaptures the worktree and rechecks the source
// identity after the artifact was built, then removes the capture indexes so
// the stage holds only the manifest and bundle.
func (e *Engine) confirmSourceUnchanged(ctx context.Context, spec SnapshotSpec, stage string, tree domain.GitOID, identity sourceIdentity) error {
	finalTree, err := e.refreshCapturedTree(ctx, spec.Source.path, filepath.Join(stage, "index-two"))
	if err != nil || finalTree != tree {
		return fmt.Errorf("%w: worktree changed during snapshot", ErrUnsafeSource)
	}
	if err := e.checkSourceIdentity(ctx, spec.Source.path, spec.Base, identity); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(stage, "index-one")); err != nil {
		return err
	}
	return os.Remove(filepath.Join(stage, "index-two"))
}

// validateSpec checks only what the Git snapshot needs before it runs. The
// identities come from Fern's own run row and are validated once, when the
// manifest is encoded.
func validateSpec(spec SnapshotSpec) error {
	if spec.Source.path == "" || spec.EpochSecond < 0 || spec.EpochSecond > 253402300799 {
		return fmt.Errorf("%w: source or epoch", ErrInvalidSpec)
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
		RepositoryID: manifest.RepositoryID, WorkspaceID: manifest.WorkspaceID, RunID: manifest.RunID,
		ResultID: manifest.ResultID, ImageIdentity: manifest.ImageIdentity,
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
