package taskenvdocker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nebler/fern/internal/atomicfile"
	"github.com/nebler/fern/internal/taskstore"
)

// cloneMarker is the private authority binding a run to the exact clone
// directory inode it created. It lives in Fern's private run root.
type cloneMarker struct {
	Version   int    `json:"version"`
	Workspace string `json:"workspace"`
	Run       string `json:"run"`
	Image     string `json:"image"`
	Clone     string `json:"clone"`
	Base      string `json:"base"`
	Remote    string `json:"remote"`
	Spec      string `json:"spec"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
}

// EnsureClone creates or reconciles the exact full independent checkout.
func (p *Provider) EnsureClone(ctx context.Context, run taskstore.BackgroundRun) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneLock(ctx, run.CloneIdentity)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	// Host Git may inspect the clone only while no run container can write it.
	// The clone lock keeps Fern from creating one until this call returns.
	if err := p.requireContainerAbsent(ctx, run, digest, ""); err != nil {
		return Observation{}, err
	}
	if err := p.admitCloneSource(ctx, run); err != nil {
		return Observation{}, err
	}
	operation, cancel := context.WithTimeout(ctx, p.config.GitTimeout)
	defer cancel()
	path := filepath.Join(p.root, run.CloneIdentity)
	var size int64
	status := "reconciled"
	if _, statErr := os.Lstat(path); statErr == nil {
		size, err = p.reconcileExistingClone(operation, run, digest, path)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Observation{}, statErr
	} else {
		var recovered bool
		status = "recovered"
		size, recovered, err = p.recoverStagedClone(operation, run, digest, path)
		if err == nil && !recovered {
			status = "created"
			size, err = p.createClone(operation, run, digest, path)
		}
	}
	if err != nil {
		return Observation{}, err
	}
	e, _ := makeEvidence(evidence{Effect: "clone", Identity: run.CloneIdentity, Spec: digest, Status: status, Detail: string(run.BaseOID), Bytes: size, Limit: p.config.CloneObservedLimitBytes})
	return Observation{Evidence: e}, nil
}

// admitCloneSource checks the operator's configured source repository and the
// clone filesystem before any clone is created or reconciled.
func (p *Provider) admitCloneSource(ctx context.Context, run taskstore.BackgroundRun) error {
	sourceBytes, err := treeSize(ctx, p.config.Repository)
	if err != nil {
		return fmt.Errorf("predict clone source size: %w", err)
	}
	if sourceBytes > p.config.SourceSizeAdmissionBytes {
		return fmt.Errorf("clone source admission predicts %d bytes, limit is %d", sourceBytes, p.config.SourceSizeAdmissionBytes)
	}
	if err := p.rejectCriticalGitSymlinks(p.config.Repository); err != nil {
		return fmt.Errorf("source Git paths are unsafe: %w", err)
	}
	if err := p.attestSourceGitConfig(ctx, run.RepositoryRemote); err != nil {
		return err
	}
	available, err := diskAvailable(p.root)
	if err != nil {
		return fmt.Errorf("inspect clone disk availability: %w", err)
	}
	if available < p.config.DiskFreeAdmissionBytes {
		return fmt.Errorf("clone disk admission requires %d bytes free, only %d available", p.config.DiskFreeAdmissionBytes, available)
	}
	return nil
}

// reconcileExistingClone attests a clone already at its canonical path.
func (p *Provider) reconcileExistingClone(ctx context.Context, run taskstore.BackgroundRun, digest, path string) (int64, error) {
	err := p.attestCloneMarker(run, digest, path)
	var size int64
	if err == nil {
		size, err = p.attestRepository(ctx, run, path)
	}
	if err != nil {
		return 0, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: err.Error()}
	}
	return size, nil
}

// recoverStagedClone finishes a publication interrupted after the marker was
// written. It reports recovered=false, after dropping any orphaned marker,
// when there is nothing to recover and a fresh clone must be created.
func (p *Provider) recoverStagedClone(ctx context.Context, run taskstore.BackgroundRun, digest, path string) (size int64, recovered bool, err error) {
	if _, err := os.Lstat(p.cloneMarkerPath(run)); errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	marker, err := p.readCloneMarker(run, digest)
	if err != nil {
		return 0, false, &IdentityError{Resource: "clone marker", Identity: run.CloneIdentity, Reason: err.Error()}
	}
	locations, unknown, err := p.findRecoverableClones(ctx, marker)
	if err != nil {
		return 0, false, err
	}
	if unknown || len(locations) > 1 {
		return 0, false, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "marker-bound clone inode has unknown or multiple recovery locations"}
	}
	if len(locations) == 0 {
		if err := p.removeCloneMarker(run, digest, marker); err != nil {
			return 0, false, fmt.Errorf("remove inode-free orphaned clone marker: %w", err)
		}
		return 0, false, nil
	}
	location := locations[0]
	if location.kind != cloneRecoveryStage {
		return 0, false, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "marker-bound clone is quarantined after interrupted cleanup"}
	}
	size, err = p.attestRepository(ctx, run, location.path)
	if err != nil {
		return 0, false, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "staged recovery failed attestation: " + err.Error()}
	}
	if err := atomicfile.RenameNoReplace(location.path, path); err != nil {
		return 0, false, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "staged recovery publication failed: " + err.Error()}
	}
	if err := atomicfile.SyncDir(p.root); err != nil {
		return 0, false, err
	}
	if err := os.Remove(location.parent); err != nil {
		return 0, false, fmt.Errorf("remove recovered clone staging directory: %w", err)
	}
	return size, true, nil
}

// createClone clones the source into a private stage, normalizes and attests
// it, writes its marker, and publishes it to path without replacement. A
// failure before publication removes the stage.
func (p *Provider) createClone(ctx context.Context, run taskstore.BackgroundRun, digest, path string) (_ int64, resultErr error) {
	stageRoot := filepath.Join(p.root, ".clone-stage-"+rand.Text())
	if err := os.Mkdir(stageRoot, 0o700); err != nil {
		return 0, fmt.Errorf("create clone staging directory: %w", err)
	}
	stageInfo, err := os.Lstat(stageRoot)
	if err != nil {
		return 0, err
	}
	published := false
	defer func() {
		if !published {
			resultErr = errors.Join(resultErr, removeCreatedTree(p.root, stageRoot, stageInfo))
		}
	}()
	stagedClone := filepath.Join(stageRoot, "clone")
	if _, err := p.git(ctx, p.root, "clone", "--no-local", "--no-hardlinks", "--no-checkout", "--", p.config.Repository, stagedClone); err != nil {
		return 0, fmt.Errorf("create independent background clone: %w", err)
	}
	for key, value := range map[string]string{"core.filemode": "true", "core.ignorecase": "false", "core.precomposeunicode": "false"} {
		if _, err := p.git(ctx, stagedClone, "config", "--local", key, value); err != nil {
			return 0, fmt.Errorf("normalize clone Git config: %w", err)
		}
	}
	if _, err := p.git(ctx, stagedClone, "checkout", "--detach", "--force", string(run.BaseOID)); err != nil {
		return 0, fmt.Errorf("detach exact base: %w", err)
	}
	if _, err := p.git(ctx, stagedClone, "remote", "set-url", "origin", run.RepositoryRemote); err != nil {
		return 0, fmt.Errorf("set canonical origin: %w", err)
	}
	if err := makeCloneWritable(stagedClone); err != nil {
		return 0, err
	}
	size, err := p.attestRepository(ctx, run, stagedClone)
	if err != nil {
		return 0, err
	}
	stagedInfo, err := os.Lstat(stagedClone)
	if err != nil {
		return 0, err
	}
	marker, err := p.writeCloneMarker(run, digest, stagedInfo)
	if err != nil {
		return 0, err
	}
	if err := atomicfile.RenameNoReplace(stagedClone, path); err != nil {
		return 0, errors.Join(fmt.Errorf("publish attested clone: %w", err), p.removeCloneMarker(run, digest, marker))
	}
	if err := atomicfile.SyncDir(p.root); err != nil {
		return 0, err
	}
	if err := os.Remove(stageRoot); err != nil {
		return 0, fmt.Errorf("remove clone staging directory: %w", err)
	}
	published = true
	return size, nil
}

func (p *Provider) cloneMarkerPath(run taskstore.BackgroundRun) string {
	return filepath.Join(p.root, ".clone-authority-"+run.CloneIdentity+".json")
}

func expectedCloneMarker(run taskstore.BackgroundRun, digest string, device, inode uint64) cloneMarker {
	return cloneMarker{1, string(run.WorkspaceID), string(run.RunID), run.ImageIdentity, run.CloneIdentity, string(run.BaseOID), run.RepositoryRemote, digest, device, inode}
}

func encodeCloneMarker(marker cloneMarker) []byte {
	data, _ := json.Marshal(marker) // fixed struct of strings and integers
	return append(data, '\n')
}

// writeCloneMarker publishes the marker for the clone directory info names. It
// never replaces an existing marker.
func (p *Provider) writeCloneMarker(run taskstore.BackgroundRun, digest string, info os.FileInfo) (cloneMarker, error) {
	device, inode, err := atomicfile.Identity(info)
	if err != nil {
		return cloneMarker{}, err
	}
	marker := expectedCloneMarker(run, digest, device, inode)
	if err := atomicfile.WriteExclusive(p.cloneMarkerPath(run), encodeCloneMarker(marker), 0o600); err != nil {
		return cloneMarker{}, fmt.Errorf("publish clone authority without replacement: %w", err)
	}
	return marker, nil
}

// readCloneMarker returns the run's marker, which must be byte-for-byte the
// marker this run would write for the inode it names.
func (p *Provider) readCloneMarker(run taskstore.BackgroundRun, digest string) (cloneMarker, error) {
	data, err := atomicfile.Read(p.cloneMarkerPath(run), maxEvidenceBytes)
	if err != nil {
		return cloneMarker{}, fmt.Errorf("read private clone authority: %w", err)
	}
	var marker cloneMarker
	if err := json.Unmarshal(data, &marker); err != nil || marker.Device == 0 || marker.Inode == 0 ||
		!bytes.Equal(data, encodeCloneMarker(expectedCloneMarker(run, digest, marker.Device, marker.Inode))) {
		return cloneMarker{}, errors.New("private clone authority does not match this run")
	}
	return marker, nil
}

func (p *Provider) attestCloneMarker(run taskstore.BackgroundRun, digest, clonePath string) error {
	marker, err := p.readCloneMarker(run, digest)
	if err != nil {
		return err
	}
	info, err := os.Lstat(clonePath)
	if err != nil {
		return errors.New("clone named by private authority is absent")
	}
	if !sameCloneIdentity(info, marker) {
		return errors.New("private clone authority names a different filesystem object")
	}
	return nil
}

// removeCloneMarker removes the marker only while it is still expected.
func (p *Provider) removeCloneMarker(run taskstore.BackgroundRun, digest string, expected cloneMarker) error {
	current, err := p.readCloneMarker(run, digest)
	if err != nil {
		return err
	}
	if current != expected {
		return errors.New("clone marker changed before removal")
	}
	if err := os.Remove(p.cloneMarkerPath(run)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicfile.SyncDir(p.root)
}

func (p *Provider) attestRepository(ctx context.Context, run taskstore.BackgroundRun, path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("clone path is not an exact directory")
	}
	if err := p.rejectCriticalGitSymlinks(path); err != nil {
		return 0, err
	}
	if err := p.attestGitConfig(ctx, path, run.RepositoryRemote); err != nil {
		return 0, err
	}
	checks := []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "--git-common-dir"}, ".git"},
		{[]string{"cat-file", "-t", string(run.BaseOID)}, "commit"},
		{[]string{"rev-parse", "HEAD"}, string(run.BaseOID)},
		{[]string{"status", "--porcelain=v2", "--untracked-files=all", "--ignored=no"}, ""},
	}
	for _, check := range checks {
		output, err := p.git(ctx, path, check.args...)
		if err != nil || strings.TrimSpace(output) != check.want {
			return 0, fmt.Errorf("Git fact %q does not match", strings.Join(check.args, " "))
		}
	}
	if err := p.requireReachableBase(ctx, path, string(run.BaseOID)); err != nil {
		return 0, err
	}
	flags, err := p.git(ctx, path, "ls-files", "-v", "-z")
	if err != nil {
		return 0, err
	}
	for _, item := range strings.Split(flags, "\x00") {
		if item == "" {
			continue
		}
		if item[0] == 'S' || (item[0] >= 'a' && item[0] <= 'z') {
			return 0, errors.New("clone index contains skip-worktree or assume-unchanged entries")
		}
	}
	if _, err := p.git(ctx, path, "symbolic-ref", "-q", "HEAD"); err == nil {
		return 0, errors.New("clone HEAD is not detached")
	}
	size, err := treeSize(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("observe clone disk use: %w", err)
	}
	if size > p.config.CloneObservedLimitBytes {
		return 0, fmt.Errorf("observed clone use is %d bytes, limit is %d", size, p.config.CloneObservedLimitBytes)
	}
	return size, nil
}

func (p *Provider) rejectCriticalGitSymlinks(path string) error {
	gitDir := filepath.Join(path, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("clone Git directory is not exact")
	}
	return filepath.WalkDir(gitDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("critical Git path is a symlink: %s", filepath.Base(path))
		}
		return nil
	})
}

func (p *Provider) attestGitConfig(ctx context.Context, path, remote string) error {
	output, err := p.git(ctx, path, "config", "--local", "--null", "--list")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, item := range strings.Split(output, "\x00") {
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "\n")
		key = strings.ToLower(key)
		if !ok || seen[key] {
			return errors.New("clone local Git config is malformed or duplicated")
		}
		seen[key] = true
		valid := false
		switch key {
		case "core.repositoryformatversion":
			valid = value == "0"
		case "core.filemode":
			valid = value == "true"
		case "core.ignorecase", "core.precomposeunicode":
			valid = value == "false"
		case "core.bare", "core.logallrefupdates":
			valid = value == map[string]string{"core.bare": "false", "core.logallrefupdates": "true"}[key]
		case "remote.origin.url":
			valid = value == remote
		case "remote.origin.fetch":
			valid = value == "+refs/heads/*:refs/remotes/origin/*"
		default:
			if strings.HasPrefix(key, "branch.") && strings.HasSuffix(key, ".remote") {
				valid = value == "origin"
			} else if strings.HasPrefix(key, "branch.") && strings.HasSuffix(key, ".merge") {
				valid = strings.HasPrefix(value, "refs/heads/") && strings.TrimSpace(value) == value && !strings.Contains(value, "..") && !strings.ContainsAny(value, "\\ ~^:?*[")
			}
		}
		if !valid {
			return fmt.Errorf("clone local Git config key %q is not allowed", key)
		}
	}
	for _, required := range []string{"core.repositoryformatversion", "core.filemode", "core.ignorecase", "core.precomposeunicode", "core.bare", "core.logallrefupdates", "remote.origin.url", "remote.origin.fetch"} {
		if !seen[required] {
			return fmt.Errorf("clone local Git config lacks %q", required)
		}
	}
	return nil
}

func (p *Provider) attestSourceGitConfig(ctx context.Context, expectedRemote string) error {
	operation, cancel := context.WithTimeout(ctx, p.config.GitTimeout)
	defer cancel()
	output, err := p.git(operation, p.config.Repository, "config", "--local", "--null", "--list")
	if err != nil {
		return fmt.Errorf("inspect source Git config: %w", err)
	}
	originURLs := 0
	for _, item := range strings.Split(output, "\x00") {
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "\n")
		key = strings.ToLower(key)
		if !ok || sourceGitConfigCanExecute(key) {
			return fmt.Errorf("source Git config key %q is not allowed for host cloning", key)
		}
		if key == "remote.origin.url" {
			originURLs++
			if value != expectedRemote && value != expectedRemote+".git" {
				return errors.New("configured source origin URL does not match the immutable run remote")
			}
		}
	}
	if originURLs != 1 {
		return fmt.Errorf("configured source must have exactly one origin fetch URL, found %d", originURLs)
	}
	return nil
}

func sourceGitConfigCanExecute(key string) bool {
	return slices.Contains([]string{
		"core.fsmonitor", "core.hookspath", "core.sshcommand", "core.gitproxy", "core.askpass", "core.pager",
		"diff.external", "interactive.difffilter", "uploadpack.packobjectshook", "sequence.editor",
	}, key) ||
		slices.ContainsFunc([]string{"alias.", "credential.", "filter.", "include.", "includeif.", "url.", "difftool.", "mergetool."}, func(prefix string) bool { return strings.HasPrefix(key, prefix) }) ||
		slices.ContainsFunc([]string{".command", ".driver", ".textconv", ".cmd", ".uploadpack", ".receivepack"}, func(suffix string) bool { return strings.HasSuffix(key, suffix) })
}

func (p *Provider) requireReachableBase(ctx context.Context, path, base string) error {
	output, err := p.git(ctx, path, "for-each-ref", "--format=%(objectname)", "refs/remotes/origin/")
	if err != nil {
		return err
	}
	reachable := false
	for _, oid := range strings.Fields(output) {
		if len(oid) != 40 && len(oid) != 64 {
			return errors.New("clone contains an invalid allowed remote ref")
		}
		if _, err := p.git(ctx, path, "merge-base", "--is-ancestor", base, oid); err == nil {
			reachable = true
			break
		}
	}
	if !reachable {
		return errors.New("base commit is not reachable from an allowed cloned remote ref")
	}
	return nil
}

func (p *Provider) git(ctx context.Context, directory string, args ...string) (string, error) {
	safe := []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.pager=cat",
		"-c", "diff.external=",
		"-c", "interactive.diffFilter=",
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"-c", "core.askPass=",
		"-c", "protocol.file.allow=always",
		"-c", "protocol.ext.allow=never",
		"-c", "fetch.writeCommitGraph=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
	}
	command := exec.CommandContext(ctx, p.config.GitExecutable, append(safe, args...)...)
	command.Dir = directory
	command.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0",
		"GIT_LFS_SKIP_SMUDGE=1", "GIT_NO_LAZY_FETCH=1", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GIT_PROTOCOL_FROM_USER=0",
		"HOME=" + p.root, "LC_ALL=C",
	}
	output := &boundedBuffer{limit: p.config.GitOutputBytes}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if output.exceeded {
		return "", errors.New("Git output exceeded configured bound")
	}
	if err != nil {
		name := "unknown"
		if len(args) > 0 {
			name = args[0]
		}
		return "", fmt.Errorf("Git %s failed: %w: %s", name, err, strings.TrimSpace(output.String()))
	}
	return output.String(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - int64(b.Len())
	if remaining <= 0 {
		b.exceeded = true
		return original, nil
	}
	if int64(len(value)) > remaining {
		value = value[:remaining]
		b.exceeded = true
	}
	_, _ = b.Buffer.Write(value)
	return original, nil
}

func makeCloneWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o777)
		}
		if info.Mode().IsRegular() {
			mode := os.FileMode(0o666)
			if info.Mode().Perm()&0o111 != 0 {
				mode = 0o777
			}
			return os.Chmod(path, mode)
		}
		return nil
	})
}

func treeSize(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// ObserveUsage returns bounded observed clone usage. This is monitoring
// evidence, not a kernel-enforced quota; Docker local-volume usage is unknown.
func (p *Provider) ObserveUsage(ctx context.Context, run taskstore.BackgroundRun) (_ UsageObservation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return UsageObservation{}, err
	}
	unlock, err := p.acquireCloneLock(ctx, run.CloneIdentity)
	if err != nil {
		return UsageObservation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	path := filepath.Join(p.root, run.CloneIdentity)
	if err := p.attestCloneDeletion(run, digest, path); err != nil {
		return UsageObservation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: err.Error()}
	}
	size, err := treeSize(ctx, path)
	if err != nil {
		return UsageObservation{}, fmt.Errorf("observe clone disk use: %w", err)
	}
	if size > p.config.CloneObservedLimitBytes {
		return UsageObservation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: fmt.Sprintf("observed clone use is %d bytes, limit is %d", size, p.config.CloneObservedLimitBytes)}
	}
	e, _ := makeEvidence(evidence{Effect: "usage", Identity: run.CloneIdentity, Spec: digest, Status: "observed", Bytes: size, Limit: p.config.CloneObservedLimitBytes})
	return UsageObservation{Evidence: e}, nil
}
