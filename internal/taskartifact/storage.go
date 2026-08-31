package taskartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store atomically installs a verified staged artifact. Repeated installation
// of byte-identical content is idempotent; any collision is rejected.
func (e *Engine) Store(ctx context.Context, staged StagedLocator) (Locator, error) {
	if err := e.validateRoots(); err != nil {
		return Locator{}, err
	}
	if staged.engine != e || staged.path == "" || !pathContains(e.casRoot, staged.path) || filepath.Dir(staged.path) != e.casRoot {
		return Locator{}, ErrInvalidLocator
	}
	info, err := os.Lstat(staged.path)
	if err != nil || !safeDirectoryInfo(info) {
		return Locator{}, fmt.Errorf("%w: staged directory", ErrStorage)
	}
	device, inode, err := fileIdentity(info)
	if err != nil || device != staged.device || inode != staged.inode {
		return Locator{}, fmt.Errorf("%w: staged identity", ErrStorage)
	}
	if err := normalizeStagedModes(staged.path); err != nil {
		return Locator{}, err
	}
	if err := validateArtifactDirectory(staged.path, 0o600); err != nil {
		return Locator{}, err
	}
	manifestBytes, err := readExactFile(filepath.Join(staged.path, manifestName), int64(e.outputBytes), 0o600)
	if err != nil {
		return Locator{}, err
	}
	if _, err := e.verifyArtifact(ctx, manifestBytes, filepath.Join(staged.path, bundleName), 0o600, staged.digest); err != nil {
		return Locator{}, err
	}
	if err := publishReadOnly(staged.path); err != nil {
		return Locator{}, err
	}
	if err := syncDirectory(staged.path); err != nil {
		_ = restoreStagedModes(staged.path)
		return Locator{}, err
	}
	target := filepath.Join(e.casRoot, staged.digest.String())
	if err := renameNoReplace(staged.path, target); err != nil {
		if _, statErr := os.Lstat(target); statErr != nil {
			_ = restoreStagedModes(staged.path)
			return Locator{}, fmt.Errorf("%w: publish CAS object", ErrStorage)
		}
		if validateErr := e.validateStoredBytes(target, staged.digest, manifestBytes, filepath.Join(staged.path, bundleName)); validateErr != nil {
			_ = restoreStagedModes(staged.path)
			return Locator{}, validateErr
		}
		if removeErr := removeExactDirectory(staged.path, staged.device, staged.inode); removeErr != nil {
			return Locator{}, removeErr
		}
	} else if err := syncDirectory(e.casRoot); err != nil {
		return Locator{}, err
	}
	return Locator{digest: staged.digest, valid: true}, nil
}

// Discard safely removes an unneeded staged artifact capability. It has no
// effect on an artifact already installed by Store.
func (e *Engine) Discard(staged StagedLocator) error {
	if err := e.validateRoots(); err != nil {
		return err
	}
	if staged.engine != e || staged.path == "" || filepath.Dir(staged.path) != e.casRoot {
		return ErrInvalidLocator
	}
	if _, err := os.Lstat(staged.path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return removeExactDirectory(staged.path, staged.device, staged.inode)
}

func (e *Engine) validateStoredBytes(target string, digest Digest, manifestBytes []byte, stagedBundle string) error {
	if err := validateArtifactDirectory(target, 0o400); err != nil {
		return err
	}
	storedManifest, err := readExactFile(filepath.Join(target, manifestName), int64(e.outputBytes), 0o400)
	if err != nil || !bytes.Equal(storedManifest, manifestBytes) || sha256Bytes(storedManifest) != digest {
		return fmt.Errorf("%w: CAS manifest collision", ErrStorage)
	}
	left, err := openCheckedArtifactFile(filepath.Join(target, bundleName), 0o400)
	if err != nil {
		return err
	}
	defer left.Close()
	right, _, err := openPrivateRead(stagedBundle, 0o400, true)
	if err != nil {
		return err
	}
	defer right.Close()
	equal, err := readersEqual(left, right)
	if err != nil || !equal {
		return fmt.Errorf("%w: CAS bundle collision", ErrStorage)
	}
	return nil
}

// Inspect revalidates both CAS bytes and the independent Git object graph.
func (e *Engine) Inspect(ctx context.Context, locator Locator) (Snapshot, error) {
	if err := e.validateRoots(); err != nil {
		return Snapshot{}, err
	}
	if !locator.valid {
		return Snapshot{}, ErrInvalidLocator
	}
	path := filepath.Join(e.casRoot, locator.digest.String())
	if err := validateArtifactDirectory(path, 0o400); err != nil {
		return Snapshot{}, err
	}
	manifestBytes, err := readExactFile(filepath.Join(path, manifestName), int64(e.outputBytes), 0o400)
	if err != nil || sha256Bytes(manifestBytes) != locator.digest {
		return Snapshot{}, fmt.Errorf("%w: manifest digest", ErrStorage)
	}
	manifest, err := e.verifyArtifact(ctx, manifestBytes, filepath.Join(path, bundleName), 0o400, locator.digest)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFromManifest(manifest, locator.digest), nil
}

// Materialize creates a fresh detached checkout under WorkRoot. The caller
// controls neither its destination nor its Git administrative data.
func (e *Engine) Materialize(ctx context.Context, locator Locator) (*Checkout, error) {
	snapshot, err := e.Inspect(ctx, locator)
	if err != nil {
		return nil, err
	}
	path, err := e.makeTemp(e.workRoot, "checkout-")
	if err != nil {
		return nil, err
	}
	pathDevice, pathInode, err := directoryIdentity(path)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = removeExactDirectory(path, pathDevice, pathInode)
		}
	}()
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	if _, err := e.gitOutput(ctx, path, nil, nil, "init"); err != nil {
		return nil, err
	}
	markerPath := checkoutMarkerPath(path)
	if err := writePrivateFile(markerPath, []byte(token+"\n")); err != nil {
		return nil, err
	}
	bundle := filepath.Join(e.casRoot, locator.digest.String(), bundleName)
	localBundle := filepath.Join(path, ".fern-artifact-bundle")
	digest, size, err := copyPrivateBundle(bundle, localBundle, e.bundleBytes, 0o400)
	if err != nil || digest != snapshot.BundleSHA256 || size != snapshot.BundleBytes {
		return nil, fmt.Errorf("%w: bundle changed", ErrCheckout)
	}
	heads, err := e.gitOutput(ctx, path, nil, nil, "bundle", "unbundle", localBundle)
	wantHeads := string(snapshot.Base) + " " + bundleBaseRef + "\n" + string(snapshot.Result) + " " + bundleResultRef + "\n"
	if err != nil || string(heads) != wantHeads {
		return nil, fmt.Errorf("%w: bundle import", ErrCheckout)
	}
	if err := os.Remove(localBundle); err != nil {
		return nil, err
	}
	if _, err := e.gitOutput(ctx, path, nil, nil, "checkout", "--detach", string(snapshot.Result)); err != nil {
		return nil, err
	}
	status, err := e.gitOutput(ctx, path, nil, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil || len(status) != 0 {
		return nil, fmt.Errorf("%w: materialized checkout is not clean", ErrCheckout)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return nil, err
	}
	device, inode, err := fileIdentity(pathInfo)
	if err != nil {
		return nil, err
	}
	markerDevice, markerInode, err := fileIdentity(markerInfo)
	if err != nil {
		return nil, err
	}
	if err := syncDirectory(path); err != nil {
		return nil, err
	}
	if err := syncDirectory(filepath.Join(path, ".git")); err != nil {
		return nil, err
	}
	keep = true
	return &Checkout{engine: e, path: path, marker: token, device: device, inode: inode, markerDevice: markerDevice, markerInode: markerInode}, nil
}

func copyPrivateBundle(source, destination string, limit int64, sourceMode os.FileMode) (Digest, int64, error) {
	input, info, err := openPrivateRead(source, sourceMode, true)
	if err != nil {
		return Digest{}, 0, err
	}
	defer input.Close()
	if info.Size() <= 0 || info.Size() > limit {
		return Digest{}, 0, ErrOutputLimit
	}
	output, err := openPrivateExclusive(destination)
	if err != nil {
		return Digest{}, 0, err
	}
	writer := &boundedHashWriter{file: output, hash: sha256.New(), limit: limit}
	_, copyErr := io.Copy(writer, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || writer.size != info.Size() {
		_ = os.Remove(destination)
		return Digest{}, 0, errors.Join(copyErr, syncErr, closeErr, ErrStorage)
	}
	var digest Digest
	copy(digest.value[:], writer.hash.Sum(nil))
	return digest, writer.size, nil
}

func (c *Checkout) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if c.engine == nil || privateRoot(c.engine.workRoot) != nil {
		return ErrCheckout
	}
	if c.engine == nil || filepath.Dir(c.path) != c.engine.workRoot || !strings.HasPrefix(filepath.Base(c.path), "checkout-") {
		return ErrCheckout
	}
	info, err := os.Lstat(c.path)
	if err != nil {
		return fmt.Errorf("%w: checkout path", ErrCheckout)
	}
	device, inode, _ := fileIdentity(info)
	markerBytes, err := readExactFile(checkoutMarkerPath(c.path), 256, 0o600)
	markerInfo, markerErr := os.Lstat(checkoutMarkerPath(c.path))
	markerDevice, markerInode, _ := fileIdentity(markerInfo)
	if err != nil || markerErr != nil || device != c.device || inode != c.inode || markerDevice != c.markerDevice || markerInode != c.markerInode || string(markerBytes) != c.marker+"\n" {
		return fmt.Errorf("%w: checkout identity", ErrCheckout)
	}
	if err := removeExactDirectory(c.path, c.device, c.inode); err != nil {
		return err
	}
	c.closed = true
	c.path = ""
	return nil
}

func checkoutMarkerPath(path string) string { return filepath.Join(path, ".git", markerName) }

func (e *Engine) validateRoots() error {
	if e == nil || privateRoot(e.casRoot) != nil || privateRoot(e.workRoot) != nil {
		return fmt.Errorf("%w: engine roots changed", ErrStorage)
	}
	return nil
}

func directoryIdentity(path string) (uint64, uint64, error) {
	info, err := os.Lstat(path)
	if err != nil || !safeDirectoryInfo(info) {
		return 0, 0, fmt.Errorf("%w: generated directory", ErrStorage)
	}
	return fileIdentity(info)
}

func validateArtifactDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil || !safeDirectoryInfo(info) {
		return fmt.Errorf("%w: CAS object directory", ErrStorage)
	}
	entries, err := os.ReadDir(path)
	// os.ReadDir sorts by filename; manifest sorts before result.
	if err != nil || len(entries) != 2 || entries[0].Name() != manifestName || entries[1].Name() != bundleName {
		return fmt.Errorf("%w: CAS object contents", ErrStorage)
	}
	for _, name := range []string{manifestName, bundleName} {
		file, err := openCheckedArtifactFile(filepath.Join(path, name), mode)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func normalizeStagedModes(path string) error {
	for _, name := range []string{manifestName, bundleName} {
		filePath := filepath.Join(path, name)
		info, err := os.Lstat(filePath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: staged file", ErrStorage)
		}
		switch info.Mode().Perm() {
		case 0o600:
		case 0o400:
			if err := changePrivateFileMode(filePath, 0o400, 0o600); err != nil {
				return fmt.Errorf("%w: restore staged file mode", ErrStorage)
			}
		default:
			return fmt.Errorf("%w: staged file mode", ErrStorage)
		}
	}
	return syncDirectory(path)
}

func publishReadOnly(path string) error {
	manifestPath := filepath.Join(path, manifestName)
	bundlePath := filepath.Join(path, bundleName)
	if err := changePrivateFileMode(manifestPath, 0o600, 0o400); err != nil {
		return fmt.Errorf("%w: make manifest read-only", ErrStorage)
	}
	if err := changePrivateFileMode(bundlePath, 0o600, 0o400); err != nil {
		_ = changePrivateFileMode(manifestPath, 0o400, 0o600)
		return fmt.Errorf("%w: make bundle read-only", ErrStorage)
	}
	return nil
}

func restoreStagedModes(path string) error {
	var result error
	for _, name := range []string{manifestName, bundleName} {
		filePath := filepath.Join(path, name)
		info, err := os.Lstat(filePath)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if info.Mode().Perm() == 0o400 {
			result = errors.Join(result, changePrivateFileMode(filePath, 0o400, 0o600))
		} else if info.Mode().Perm() != 0o600 {
			result = errors.Join(result, ErrStorage)
		}
	}
	return errors.Join(result, syncDirectory(path))
}

func openCheckedArtifactFile(path string, mode os.FileMode) (*os.File, error) {
	file, _, err := openPrivateRead(path, mode, true)
	if err != nil {
		return nil, fmt.Errorf("%w: artifact file", ErrStorage)
	}
	return file, nil
}

func readExactFile(path string, limit int64, mode os.FileMode) ([]byte, error) {
	file, info, err := openPrivateRead(path, mode, true)
	if err != nil {
		return nil, fmt.Errorf("%w: artifact file", ErrStorage)
	}
	defer file.Close()
	if info.Size() < 0 || info.Size() > limit {
		return nil, ErrOutputLimit
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(value)) != info.Size() {
		return nil, fmt.Errorf("%w: artifact read", ErrStorage)
	}
	return value, nil
}

func readersEqual(left, right io.Reader) (bool, error) {
	leftBuffer := make([]byte, 32<<10)
	rightBuffer := make([]byte, 32<<10)
	for {
		leftN, leftErr := io.ReadFull(left, leftBuffer)
		rightN, rightErr := io.ReadFull(right, rightBuffer)
		if leftN != rightN || !bytes.Equal(leftBuffer[:leftN], rightBuffer[:rightN]) {
			return false, nil
		}
		if errors.Is(leftErr, io.EOF) || errors.Is(leftErr, io.ErrUnexpectedEOF) || errors.Is(rightErr, io.EOF) || errors.Is(rightErr, io.ErrUnexpectedEOF) {
			return leftN == rightN && (errors.Is(leftErr, io.EOF) || errors.Is(leftErr, io.ErrUnexpectedEOF)) && (errors.Is(rightErr, io.EOF) || errors.Is(rightErr, io.ErrUnexpectedEOF)), nil
		}
		if leftErr != nil || rightErr != nil {
			return false, errors.Join(leftErr, rightErr)
		}
	}
}
