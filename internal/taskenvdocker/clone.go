package taskenvdocker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nebler/fern/internal/taskstore"
)

type cloneMarker struct {
	Version    int    `json:"version"`
	Workspace  string `json:"workspace"`
	Task       string `json:"task"`
	Attempt    string `json:"attempt"`
	Generation int64  `json:"generation"`
	Image      string `json:"image"`
	Clone      string `json:"clone"`
	Base       string `json:"base"`
	Remote     string `json:"remote"`
	Spec       string `json:"spec"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
}

type cloneMarkerSnapshot struct {
	marker                    cloneMarker
	markerDevice, markerInode uint64
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
	path := filepath.Join(p.root, run.CloneIdentity)
	markerPath := p.cloneMarkerPath(run)
	_, statErr := os.Lstat(path)
	if statErr == nil {
		if err := p.requireNoRunContainer(ctx, run, digest); err != nil {
			return Observation{}, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Observation{}, statErr
	}
	sourceBytes, err := treeSize(p.config.Repository)
	if err != nil {
		return Observation{}, fmt.Errorf("predict clone source size: %w", err)
	}
	if sourceBytes > p.config.SourceSizeAdmissionBytes {
		return Observation{}, fmt.Errorf("clone source admission predicts %d bytes, limit is %d", sourceBytes, p.config.SourceSizeAdmissionBytes)
	}
	if err := p.rejectCriticalGitSymlinks(p.config.Repository); err != nil {
		return Observation{}, fmt.Errorf("source Git paths are unsafe: %w", err)
	}
	if err := p.attestSourceGitConfig(ctx, run.RepositoryRemote); err != nil {
		return Observation{}, err
	}
	available, err := diskAvailable(p.root)
	if err != nil {
		return Observation{}, fmt.Errorf("inspect clone disk availability: %w", err)
	}
	if available < p.config.DiskFreeAdmissionBytes {
		return Observation{}, fmt.Errorf("clone disk admission requires %d bytes free, only %d available", p.config.DiskFreeAdmissionBytes, available)
	}

	if statErr == nil {
		size, err := p.attestClone(ctx, run, digest, path, true)
		if err != nil {
			return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: err.Error()}
		}
		e, _ := makeEvidence(evidence{Effect: "clone", Identity: run.CloneIdentity, Spec: digest, Status: "reconciled", Detail: string(run.BaseOID), Bytes: size, Limit: p.config.CloneObservedLimitBytes})
		return Observation{Evidence: e}, nil
	}
	if _, err := os.Lstat(markerPath); err == nil {
		snapshot, err := p.readCloneMarkerSnapshot(run, digest)
		if err != nil {
			return Observation{}, &IdentityError{Resource: "clone marker", Identity: run.CloneIdentity, Reason: err.Error()}
		}
		locations, unknown, err := p.findRecoverableClones(snapshot.marker)
		if err != nil {
			return Observation{}, err
		}
		if unknown || len(locations) > 1 {
			return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "marker-bound clone inode has unknown or multiple recovery locations"}
		}
		if len(locations) == 1 {
			location := locations[0]
			if location.kind != cloneRecoveryStage {
				return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "marker-bound clone is quarantined after interrupted cleanup"}
			}
			if err := p.requireNoRunContainer(ctx, run, digest); err != nil {
				return Observation{}, err
			}
			operation, cancel := operationContext(ctx, p.config.GitTimeout)
			defer cancel()
			size, err := p.attestRepository(operation, run, location.path, true)
			if err != nil {
				return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "staged recovery failed attestation: " + err.Error()}
			}
			if err := renameNoReplace(location.path, path); err != nil {
				return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "staged recovery publication failed: " + err.Error()}
			}
			if err := syncDirectory(p.root); err != nil {
				return Observation{}, err
			}
			if err := os.Remove(location.parent); err != nil {
				return Observation{}, fmt.Errorf("remove recovered clone staging directory: %w", err)
			}
			e, _ := makeEvidence(evidence{Effect: "clone", Identity: run.CloneIdentity, Spec: digest, Status: "recovered", Detail: string(run.BaseOID), Bytes: size, Limit: p.config.CloneObservedLimitBytes})
			return Observation{Evidence: e}, nil
		}
		if err := p.removeExactCloneMarker(run, digest, snapshot, snapshot.marker.Device, snapshot.marker.Inode); err != nil {
			return Observation{}, fmt.Errorf("remove inode-free orphaned clone marker: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Observation{}, err
	}

	suffix, err := randomSuffix()
	if err != nil {
		return Observation{}, err
	}
	stageRoot := filepath.Join(p.root, ".clone-stage-"+suffix)
	if err := os.Mkdir(stageRoot, 0o700); err != nil {
		return Observation{}, fmt.Errorf("create clone staging directory: %w", err)
	}
	stageInfo, err := os.Lstat(stageRoot)
	if err != nil {
		return Observation{}, err
	}
	stageLive := true
	defer func() {
		if stageLive {
			resultErr = errors.Join(resultErr, removeCreatedTree(p.root, stageRoot, stageInfo))
		}
	}()
	stagedClone := filepath.Join(stageRoot, "clone")
	operation, cancel := operationContext(ctx, p.config.GitTimeout)
	defer cancel()
	if _, err := p.git(operation, p.root, "clone", "--no-local", "--no-hardlinks", "--no-checkout", "--", p.config.Repository, stagedClone); err != nil {
		return Observation{}, fmt.Errorf("create independent background clone: %w", err)
	}
	if err := p.rejectCriticalGitSymlinks(stagedClone); err != nil {
		return Observation{}, err
	}
	for key, value := range map[string]string{"core.filemode": "true", "core.ignorecase": "false", "core.precomposeunicode": "false"} {
		if _, err := p.git(operation, stagedClone, "config", "--local", key, value); err != nil {
			return Observation{}, fmt.Errorf("normalize clone Git config: %w", err)
		}
	}
	if err := p.attestGitConfig(operation, stagedClone, p.config.Repository); err != nil {
		return Observation{}, err
	}
	if err := p.requireReachableBase(operation, stagedClone, string(run.BaseOID)); err != nil {
		return Observation{}, err
	}
	if _, err := p.git(operation, stagedClone, "checkout", "--detach", "--force", string(run.BaseOID)); err != nil {
		return Observation{}, fmt.Errorf("detach exact base: %w", err)
	}
	if _, err := p.git(operation, stagedClone, "remote", "set-url", "origin", run.RepositoryRemote); err != nil {
		return Observation{}, fmt.Errorf("set canonical origin: %w", err)
	}
	if err := makeCloneWritable(stagedClone); err != nil {
		return Observation{}, err
	}
	if _, err := p.attestRepository(operation, run, stagedClone, true); err != nil {
		return Observation{}, err
	}
	stagedInfo, err := os.Lstat(stagedClone)
	if err != nil {
		return Observation{}, err
	}
	markerSnapshot, err := p.writeCloneMarker(run, digest, stagedInfo)
	if err != nil {
		return Observation{}, err
	}
	if err := renameNoReplace(stagedClone, path); err != nil {
		device, inode, identityErr := fileIdentity(stagedInfo)
		markerErr := p.removeExactCloneMarker(run, digest, markerSnapshot, device, inode)
		return Observation{}, errors.Join(fmt.Errorf("publish attested clone: %w", err), identityErr, markerErr)
	}
	if err := syncDirectory(p.root); err != nil {
		return Observation{}, err
	}
	if err := os.Remove(stageRoot); err != nil {
		return Observation{}, fmt.Errorf("remove clone staging directory: %w", err)
	}
	stageLive = false
	size, err := p.attestClone(operation, run, digest, path, true)
	if err != nil {
		return Observation{}, err
	}
	e, _ := makeEvidence(evidence{Effect: "clone", Identity: run.CloneIdentity, Spec: digest, Status: "created", Detail: string(run.BaseOID), Bytes: size, Limit: p.config.CloneObservedLimitBytes})
	return Observation{Evidence: e}, nil
}

func (p *Provider) cloneMarkerPath(run taskstore.BackgroundRun) string {
	return filepath.Join(p.root, ".clone-authority-"+run.CloneIdentity+".json")
}

func expectedCloneMarker(run taskstore.BackgroundRun, digest string, device, inode uint64) cloneMarker {
	return cloneMarker{1, string(run.WorkspaceID), string(run.TaskID), string(run.AttemptID), run.Generation, run.ImageIdentity, run.CloneIdentity, string(run.BaseOID), run.RepositoryRemote, digest, device, inode}
}

func (p *Provider) writeCloneMarker(run taskstore.BackgroundRun, digest string, info os.FileInfo) (cloneMarkerSnapshot, error) {
	device, inode, err := fileIdentity(info)
	if err != nil {
		return cloneMarkerSnapshot{}, err
	}
	data, err := json.Marshal(expectedCloneMarker(run, digest, device, inode))
	if err != nil {
		return cloneMarkerSnapshot{}, err
	}
	data = append(data, '\n')
	suffix, err := randomSuffix()
	if err != nil {
		return cloneMarkerSnapshot{}, err
	}
	temporary := filepath.Join(p.root, ".clone-marker-stage-"+suffix)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return cloneMarkerSnapshot{}, fmt.Errorf("create staged clone authority: %w", err)
	}
	writeErr := func() error {
		if _, err := file.Write(data); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		return cloneMarkerSnapshot{}, errors.Join(writeErr, closeErr)
	}
	if err := renameNoReplace(temporary, p.cloneMarkerPath(run)); err != nil {
		_ = os.Remove(temporary)
		return cloneMarkerSnapshot{}, fmt.Errorf("publish clone authority without replacement: %w", err)
	}
	if err := syncDirectory(p.root); err != nil {
		return cloneMarkerSnapshot{}, err
	}
	snapshot, err := p.readCloneMarkerSnapshot(run, digest)
	if err != nil {
		return cloneMarkerSnapshot{}, err
	}
	return snapshot, nil
}

func (p *Provider) readCloneMarker(run taskstore.BackgroundRun, digest string) (cloneMarker, error) {
	snapshot, err := p.readCloneMarkerSnapshot(run, digest)
	return snapshot.marker, err
}

func (p *Provider) readCloneMarkerSnapshot(run taskstore.BackgroundRun, digest string) (cloneMarkerSnapshot, error) {
	path := p.cloneMarkerPath(run)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() > maxEvidenceBytes {
		return cloneMarkerSnapshot{}, errors.New("private clone authority is absent or unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cloneMarkerSnapshot{}, errors.New("private clone authority cannot be read")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) {
		return cloneMarkerSnapshot{}, errors.New("private clone authority changed while being read")
	}
	var marker cloneMarker
	if err := json.Unmarshal(data, &marker); err != nil || marker.Device == 0 || marker.Inode == 0 {
		return cloneMarkerSnapshot{}, errors.New("private clone authority is malformed")
	}
	want, _ := json.Marshal(expectedCloneMarker(run, digest, marker.Device, marker.Inode))
	want = append(want, '\n')
	if !bytes.Equal(data, want) {
		return cloneMarkerSnapshot{}, errors.New("private clone authority does not match")
	}
	markerDevice, markerInode, err := fileIdentity(after)
	if err != nil {
		return cloneMarkerSnapshot{}, err
	}
	return cloneMarkerSnapshot{marker: marker, markerDevice: markerDevice, markerInode: markerInode}, nil
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
	device, inode, err := fileIdentity(info)
	if err != nil || marker.Device != device || marker.Inode != inode {
		return errors.New("private clone authority names a different filesystem object")
	}
	return nil
}

func (p *Provider) removeExactCloneMarker(run taskstore.BackgroundRun, digest string, expected cloneMarkerSnapshot, cloneDevice, cloneInode uint64) error {
	if expected.marker.Device != cloneDevice || expected.marker.Inode != cloneInode {
		return errors.New("clone marker removal authority names a different clone inode")
	}
	current, err := p.readCloneMarkerSnapshot(run, digest)
	if err != nil {
		return err
	}
	if current != expected {
		return errors.New("clone marker changed before exact removal")
	}
	if err := os.Remove(p.cloneMarkerPath(run)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(p.root)
}

func (p *Provider) attestClone(ctx context.Context, run taskstore.BackgroundRun, digest, path string, requireBase bool) (int64, error) {
	if err := p.attestCloneMarker(run, digest, path); err != nil {
		return 0, err
	}
	if err := p.requireNoRunContainer(ctx, run, digest); err != nil {
		return 0, err
	}
	return p.attestRepository(ctx, run, path, requireBase)
}

func (p *Provider) attestRepository(ctx context.Context, run taskstore.BackgroundRun, path string, requireBase bool) (int64, error) {
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
	}
	if requireBase {
		checks = append(checks,
			struct {
				args []string
				want string
			}{[]string{"rev-parse", "HEAD"}, string(run.BaseOID)},
			struct {
				args []string
				want string
			}{[]string{"status", "--porcelain=v2", "--untracked-files=all", "--ignored=no"}, ""},
		)
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
	if requireBase {
		if _, err := p.git(ctx, path, "symbolic-ref", "-q", "HEAD"); err == nil {
			return 0, errors.New("clone HEAD is not detached")
		}
	}
	size, err := treeSize(path)
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
	operation, cancel := operationContext(ctx, p.config.GitTimeout)
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
			if value != expectedRemote {
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
	for _, exact := range []string{
		"core.fsmonitor", "core.hookspath", "core.sshcommand", "core.gitproxy", "core.askpass", "core.pager",
		"diff.external", "interactive.difffilter", "uploadpack.packobjectshook", "sequence.editor",
	} {
		if key == exact {
			return true
		}
	}
	for _, prefix := range []string{"alias.", "credential.", "filter.", "include.", "includeif.", "url.", "difftool.", "mergetool."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, suffix := range []string{".command", ".driver", ".textconv", ".cmd", ".uploadpack", ".receivepack"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
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

type cloneRecoveryKind uint8

const (
	cloneRecoveryStage cloneRecoveryKind = iota + 1
	cloneRecoveryQuarantine
)

type cloneRecoveryLocation struct {
	kind   cloneRecoveryKind
	path   string
	parent string
}

func (p *Provider) findRecoverableClones(marker cloneMarker) ([]cloneRecoveryLocation, bool, error) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return nil, false, err
	}
	var locations []cloneRecoveryLocation
	unknown := false
	for _, entry := range entries {
		path := filepath.Join(p.root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, false, err
		}
		if sameCloneIdentity(info, marker) {
			if validRecoveryName(entry.Name(), ".clone-quarantine-") && entry.IsDir() {
				locations = append(locations, cloneRecoveryLocation{kind: cloneRecoveryQuarantine, path: path, parent: p.root})
			} else if entry.Name() != marker.Clone {
				unknown = true
			}
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		candidate := filepath.Join(path, "clone")
		candidateInfo, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if !sameCloneIdentity(candidateInfo, marker) {
			continue
		}
		if validRecoveryName(entry.Name(), ".clone-stage-") && candidateInfo.IsDir() && candidateInfo.Mode()&os.ModeSymlink == 0 {
			locations = append(locations, cloneRecoveryLocation{kind: cloneRecoveryStage, path: candidate, parent: path})
		} else {
			unknown = true
		}
	}
	known := make(map[string]bool, len(locations))
	for _, location := range locations {
		known[location.path] = true
	}
	err = filepath.WalkDir(p.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() || path == p.root {
			return nil
		}
		if known[path] {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if sameCloneIdentity(info, marker) {
			unknown = true
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return locations, unknown, nil
}

func sameCloneIdentity(info os.FileInfo, marker cloneMarker) bool {
	device, inode, err := fileIdentity(info)
	return err == nil && device == marker.Device && inode == marker.Inode
}

func validRecoveryName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) || len(name) != len(prefix)+12 {
		return false
	}
	for _, value := range name[len(prefix):] {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz234567", value) {
			return false
		}
	}
	return true
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

func treeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
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

func removeCreatedTree(root, path string, expected os.FileInfo) error {
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	quarantine := filepath.Join(root, ".clone-quarantine-"+suffix)
	if err := os.Rename(path, quarantine); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("quarantine created clone tree: %w", err)
	}
	got, err := os.Lstat(quarantine)
	if err != nil || !os.SameFile(expected, got) {
		return errors.New("quarantined clone tree is not the created inode")
	}
	if err := os.RemoveAll(quarantine); err != nil {
		return err
	}
	return syncDirectory(root)
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
	size, err := treeSize(path)
	if err != nil {
		return UsageObservation{}, fmt.Errorf("observe clone disk use: %w", err)
	}
	if size > p.config.CloneObservedLimitBytes {
		return UsageObservation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: fmt.Sprintf("observed clone use is %d bytes, limit is %d", size, p.config.CloneObservedLimitBytes)}
	}
	e, _ := makeEvidence(evidence{Effect: "usage", Identity: run.CloneIdentity, Spec: digest, Status: "observed", Bytes: size, Limit: p.config.CloneObservedLimitBytes})
	return UsageObservation{Evidence: e, CloneBytes: size, ObservedLimitBytes: p.config.CloneObservedLimitBytes, VolumeBytesAvailable: false}, nil
}

// RemoveClone removes only an exactly attested clone after the exact runtime is absent.
func (p *Provider) RemoveClone(ctx context.Context, run taskstore.BackgroundRun, authority CleanupAuthority) (_ Observation, resultErr error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return Observation{}, err
	}
	if _, err := validateCleanupAuthority(authority); err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneLock(ctx, run.CloneIdentity)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	if err := p.requireContainerAbsent(ctx, run, digest, authority); err != nil {
		return Observation{}, err
	}
	path := filepath.Join(p.root, run.CloneIdentity)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, markerErr := os.Lstat(p.cloneMarkerPath(run)); markerErr == nil {
			recovered, markerErr := p.finishMarkerBoundDeletion(run, digest)
			if markerErr != nil {
				return Observation{}, &IdentityError{Resource: "clone marker", Identity: run.CloneIdentity, Reason: markerErr.Error()}
			}
			status := "absent"
			if recovered {
				status = "recovered"
			}
			e, _ := makeEvidence(evidence{Effect: "clone_remove", Identity: run.CloneIdentity, Spec: digest, Status: status})
			return Observation{Evidence: e}, nil
		} else if !errors.Is(markerErr, os.ErrNotExist) {
			return Observation{}, markerErr
		}
		e, _ := makeEvidence(evidence{Effect: "clone_remove", Identity: run.CloneIdentity, Spec: digest, Status: "absent"})
		return Observation{Evidence: e}, nil
	}
	if err != nil {
		return Observation{}, err
	}
	if err := p.attestCloneDeletion(run, digest, path); err != nil {
		return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: err.Error()}
	}
	suffix, err := randomSuffix()
	if err != nil {
		return Observation{}, err
	}
	quarantine := filepath.Join(p.root, ".clone-quarantine-"+suffix)
	if err := os.Rename(path, quarantine); err != nil {
		return Observation{}, fmt.Errorf("quarantine clone before removal: %w", err)
	}
	quarantinedInfo, err := os.Lstat(quarantine)
	if err != nil || !os.SameFile(info, quarantinedInfo) {
		return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "renamed clone is not the attested inode"}
	}
	if err := p.attestCloneDeletion(run, digest, quarantine); err != nil {
		return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "renamed clone failed re-attestation: " + err.Error()}
	}
	if err := os.RemoveAll(quarantine); err != nil {
		return Observation{}, err
	}
	if _, err := p.finishMarkerBoundDeletion(run, digest); err != nil {
		return Observation{}, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return Observation{}, errors.New("canonical clone path was replaced during removal and remains quarantined")
	}
	e, _ := makeEvidence(evidence{Effect: "clone_remove", Identity: run.CloneIdentity, Spec: digest, Status: "removed"})
	return Observation{Evidence: e}, nil
}

func (p *Provider) finishMarkerBoundDeletion(run taskstore.BackgroundRun, digest string) (bool, error) {
	snapshot, err := p.readCloneMarkerSnapshot(run, digest)
	if err != nil {
		return false, err
	}
	marker := snapshot.marker
	locations, unknown, err := p.findRecoverableClones(marker)
	if err != nil {
		return false, err
	}
	if unknown || len(locations) > 1 {
		return false, errors.New("marker-bound clone inode has unknown or multiple recovery locations")
	}
	recovered := len(locations) == 1
	if recovered {
		location := locations[0]
		if err := p.attestCloneMarker(run, digest, location.path); err != nil {
			return false, err
		}
		if err := os.RemoveAll(location.path); err != nil {
			return false, err
		}
		if location.kind == cloneRecoveryStage {
			if err := os.Remove(location.parent); err != nil {
				return false, fmt.Errorf("remove recovered staging parent: %w", err)
			}
		}
		locations, unknown, err = p.findRecoverableClones(marker)
		if err != nil {
			return false, err
		}
	}
	if unknown || len(locations) != 0 {
		return false, errors.New("marker-bound clone inode remains after deletion")
	}
	if err := p.removeExactCloneMarker(run, digest, snapshot, marker.Device, marker.Inode); err != nil {
		return false, err
	}
	return recovered, nil
}

func (p *Provider) attestCloneDeletion(run taskstore.BackgroundRun, digest, path string) error {
	if err := p.attestCloneMarker(run, digest, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("clone deletion target is not an exact directory")
	}
	return nil
}
