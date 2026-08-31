package taskartifact

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	bundleBaseRef   = "refs/fern-artifact/base"
	bundleResultRef = "refs/fern-artifact/result"
)

func (e *Engine) verifyArtifact(ctx context.Context, manifestBytes []byte, bundlePath string, fileMode os.FileMode, expectedDigest Digest) (artifactManifest, error) {
	manifest, digest, err := decodeManifest(manifestBytes)
	if err != nil || digest != expectedDigest || manifest.BundleBytes > e.bundleBytes || len(manifest.Changes) > e.manifestFiles {
		return artifactManifest{}, fmt.Errorf("%w: manifest digest or bounds", ErrVerification)
	}
	verifyRoot, err := e.makeTemp(e.workRoot, ".verify-")
	if err != nil {
		return artifactManifest{}, err
	}
	verifyDevice, verifyInode, err := directoryIdentity(verifyRoot)
	if err != nil {
		return artifactManifest{}, err
	}
	defer removeExactDirectory(verifyRoot, verifyDevice, verifyInode)
	verifiedBundle := filepath.Join(verifyRoot, bundleName)
	bundleDigest, bundleSize, err := copyPrivateBundle(bundlePath, verifiedBundle, e.bundleBytes, fileMode)
	if err != nil || bundleDigest != manifest.BundleSHA256 || bundleSize != manifest.BundleBytes {
		return artifactManifest{}, fmt.Errorf("%w: bundle digest", ErrVerification)
	}
	repository := filepath.Join(verifyRoot, "repository.git")
	if _, err := e.gitOutput(ctx, verifyRoot, nil, nil, "init", "--bare", "--object-format=sha1", repository); err != nil {
		return artifactManifest{}, err
	}
	if _, err := e.gitOutput(ctx, repository, nil, nil, "bundle", "verify", verifiedBundle); err != nil {
		return artifactManifest{}, fmt.Errorf("%w: bundle verify", ErrVerification)
	}
	heads, err := e.gitOutput(ctx, repository, nil, nil, "bundle", "list-heads", verifiedBundle)
	if err != nil || string(heads) != string(manifest.Base)+" "+bundleBaseRef+"\n"+string(manifest.Result)+" "+bundleResultRef+"\n" {
		return artifactManifest{}, fmt.Errorf("%w: bundle heads", ErrVerification)
	}
	unbundled, err := e.gitOutput(ctx, repository, nil, nil, "bundle", "unbundle", verifiedBundle)
	if err != nil || !bytes.Equal(unbundled, heads) {
		return artifactManifest{}, fmt.Errorf("%w: bundle import", ErrVerification)
	}
	zero := strings.Repeat("0", 40)
	if _, err := e.gitOutput(ctx, repository, nil, nil, "update-ref", "refs/verify/base", string(manifest.Base), zero); err != nil {
		return artifactManifest{}, err
	}
	if _, err := e.gitOutput(ctx, repository, nil, nil, "update-ref", "refs/verify/result", string(manifest.Result), zero); err != nil {
		return artifactManifest{}, err
	}
	fsck, err := e.gitOutput(ctx, repository, nil, nil, "fsck", "--strict", "--full", "--no-reflogs", "--unreachable")
	if err != nil || len(fsck) != 0 {
		return artifactManifest{}, fmt.Errorf("%w: object graph", ErrVerification)
	}
	for _, oid := range []string{string(manifest.Base), string(manifest.Result)} {
		typeOutput, err := e.gitOutput(ctx, repository, nil, nil, "cat-file", "-t", oid)
		if err != nil || string(typeOutput) != "commit\n" {
			return artifactManifest{}, fmt.Errorf("%w: commit object", ErrVerification)
		}
	}
	tree, err := e.oid(ctx, repository, string(manifest.Result)+"^{tree}", nil)
	if err != nil || tree != manifest.Tree {
		return artifactManifest{}, fmt.Errorf("%w: result tree", ErrVerification)
	}
	if err := e.proveTree(ctx, repository, manifest.Base); err != nil {
		return artifactManifest{}, fmt.Errorf("%w: base tree", ErrVerification)
	}
	if err := e.proveTree(ctx, repository, manifest.Result); err != nil {
		return artifactManifest{}, fmt.Errorf("%w: result tree", ErrVerification)
	}
	if manifest.Result != manifest.Base {
		commit, err := e.gitOutput(ctx, repository, nil, nil, "cat-file", "commit", string(manifest.Result))
		if err != nil || !bytes.Equal(commit, canonicalCommit(manifest)) {
			return artifactManifest{}, fmt.Errorf("%w: normalized commit", ErrVerification)
		}
	}
	changes, err := e.buildChanges(ctx, repository, manifest.Base, manifest.Result)
	if err != nil || !manifestsEqual(changes, manifest.Changes) {
		return artifactManifest{}, fmt.Errorf("%w: rebuilt manifest", ErrVerification)
	}
	_, changesDigest, err := canonicalChanges(changes)
	if err != nil || changesDigest != manifest.ChangesSHA256 {
		return artifactManifest{}, fmt.Errorf("%w: rebuilt changes digest", ErrVerification)
	}
	return manifest, nil
}

func canonicalCommit(manifest artifactManifest) []byte {
	epoch := strconv.FormatInt(manifest.EpochSecond, 10)
	return []byte("tree " + string(manifest.Tree) + "\nparent " + string(manifest.Base) +
		"\nauthor Fern Artifact <artifact@fern.invalid> " + epoch + " +0000\ncommitter Fern Artifact <artifact@fern.invalid> " + epoch +
		" +0000\n\nFern retained background run artifact\n")
}
