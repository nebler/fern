package taskartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nebler/fern/internal/gitref"
	"github.com/nebler/fern/internal/task"
)

type sourceIdentity struct {
	rootDevice, rootInode uint64
	gitDevice, gitInode   uint64
	head                  task.GitOID
	indexDigest           Digest
	indexSize             int64
}

func (e *Engine) admitSource(ctx context.Context, repository string, base task.GitOID) (sourceIdentity, error) {
	var identity sourceIdentity
	root, err := exactDirectory(repository, false)
	if err != nil {
		return identity, fmt.Errorf("%w: repository path", ErrUnsafeSource)
	}
	gitDirectory := filepath.Join(repository, ".git")
	gitInfo, err := exactDirectory(gitDirectory, false)
	if err != nil {
		return identity, fmt.Errorf("%w: .git directory", ErrUnsafeSource)
	}
	identity.rootDevice, identity.rootInode, err = fileIdentity(root)
	if err != nil {
		return identity, err
	}
	identity.gitDevice, identity.gitInode, err = fileIdentity(gitInfo)
	if err != nil {
		return identity, err
	}
	if err := rejectAdministrativeSymlinks(gitDirectory); err != nil {
		return identity, err
	}
	for _, relative := range []string{"commondir", "shallow", "info/grafts", "objects/info/alternates", "objects/info/http-alternates", "info/sparse-checkout"} {
		if _, err := os.Lstat(filepath.Join(gitDirectory, filepath.FromSlash(relative))); err == nil || !errors.Is(err, os.ErrNotExist) {
			return identity, fmt.Errorf("%w: unsupported repository structure", ErrUnsafeSource)
		}
	}
	for _, relative := range []string{"objects", "objects/info", "objects/pack", "refs", "hooks"} {
		path := filepath.Join(gitDirectory, filepath.FromSlash(relative))
		if _, err := exactDirectory(path, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return identity, fmt.Errorf("%w: unsafe administrative path", ErrUnsafeSource)
		}
	}
	packEntries, err := os.ReadDir(filepath.Join(gitDirectory, "objects", "pack"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return identity, fmt.Errorf("%w: object packs", ErrUnsafeSource)
	}
	for _, entry := range packEntries {
		if strings.HasSuffix(entry.Name(), ".promisor") {
			return identity, fmt.Errorf("%w: promisor objects", ErrUnsafeSource)
		}
	}
	if err := rejectHooks(gitDirectory); err != nil {
		return identity, err
	}
	config, err := e.gitOutput(ctx, repository, nil, nil, "config", "--local", "--no-includes", "--null", "--list")
	if err != nil {
		return identity, err
	}
	if err := validateLocalConfig(config); err != nil {
		return identity, err
	}
	checks := []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "--show-toplevel"}, repository + "\n"},
		{[]string{"rev-parse", "--absolute-git-dir"}, gitDirectory + "\n"},
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, gitDirectory + "\n"},
		{[]string{"rev-parse", "--is-inside-work-tree"}, "true\n"},
		{[]string{"rev-parse", "--is-shallow-repository"}, "false\n"},
		{[]string{"rev-parse", "--show-object-format"}, "sha1\n"},
	}
	for _, check := range checks {
		output, err := e.gitOutput(ctx, repository, nil, nil, check.args...)
		if err != nil || string(output) != check.want {
			return identity, fmt.Errorf("%w: repository identity", ErrUnsafeSource)
		}
	}
	identity.head, err = e.oid(ctx, repository, "HEAD^{commit}", nil)
	if err != nil {
		return identity, fmt.Errorf("%w: HEAD identity", ErrUnsafeSource)
	}
	if _, err := e.gitOutput(ctx, repository, nil, nil, "merge-base", "--is-ancestor", string(base), string(identity.head)); err != nil {
		return identity, fmt.Errorf("%w: admitted base is not an ancestor of HEAD", ErrUnsafeSource)
	}
	replacements, err := e.gitOutput(ctx, repository, nil, nil, "for-each-ref", "--format=%(refname)", "refs/replace")
	if err != nil || len(replacements) != 0 {
		return identity, fmt.Errorf("%w: replacement refs", ErrUnsafeSource)
	}
	if err := e.proveArtifactRefsEmpty(ctx, repository); err != nil {
		return identity, err
	}
	if err := e.proveTree(ctx, repository, base); err != nil {
		return identity, err
	}
	if err := e.proveTree(ctx, repository, identity.head); err != nil {
		return identity, err
	}
	identity.indexDigest, identity.indexSize, err = hashRegularFile(filepath.Join(gitDirectory, "index"), int64(e.outputBytes))
	if err != nil {
		return identity, fmt.Errorf("%w: index", ErrUnsafeSource)
	}
	return identity, nil
}

func rejectAdministrativeSymlinks(gitDirectory string) error {
	return filepath.WalkDir(gitDirectory, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: unreadable administrative path", ErrUnsafeSource)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: administrative symlink", ErrUnsafeSource)
		}
		return nil
	})
}

func rejectHooks(gitDirectory string) error {
	entries, err := os.ReadDir(filepath.Join(gitDirectory, "hooks"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: hooks", ErrUnsafeSource)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sample") {
			return fmt.Errorf("%w: repository hook", ErrUnsafeSource)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: repository hook", ErrUnsafeSource)
		}
	}
	return nil
}

func validateLocalConfig(output []byte) error {
	if len(output) > 0 && output[len(output)-1] != 0 {
		return fmt.Errorf("%w: malformed local config", ErrUnsafeSource)
	}
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		separator := bytes.IndexByte(record, '\n')
		if separator <= 0 {
			return fmt.Errorf("%w: malformed local config", ErrUnsafeSource)
		}
		key := strings.ToLower(string(record[:separator]))
		value := strings.ToLower(string(record[separator+1:]))
		dangerous := strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.") ||
			key == "core.worktree" || key == "core.hookspath" || key == "core.fsmonitor" || key == "core.attributesfile" ||
			key == "core.excludesfile" || key == "core.alternaterefscommand" || key == "core.sparsecheckout" ||
			key == "core.sparsecheckoutcone" || key == "extensions.worktreeconfig" || key == "extensions.partialclone" ||
			key == "diff.external" || strings.HasPrefix(key, "filter.") ||
			(strings.HasPrefix(key, "diff.") && strings.HasSuffix(key, ".command")) ||
			(strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".promisor") || strings.HasSuffix(key, ".partialclonefilter"))) ||
			strings.HasPrefix(key, "submodule.") ||
			(strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof"))) ||
			key == "core.sharedrepository" && value != "0" && value != "false" && value != "umask"
		if dangerous {
			return fmt.Errorf("%w: unsafe local config", ErrUnsafeSource)
		}
	}
	return nil
}

func (e *Engine) checkSourceIdentity(ctx context.Context, repository string, base task.GitOID, expected sourceIdentity) error {
	root, err := os.Lstat(repository)
	if err != nil {
		return fmt.Errorf("%w: source disappeared", ErrUnsafeSource)
	}
	gitInfo, err := os.Lstat(filepath.Join(repository, ".git"))
	if err != nil {
		return fmt.Errorf("%w: source disappeared", ErrUnsafeSource)
	}
	rootDevice, rootInode, _ := fileIdentity(root)
	gitDevice, gitInode, _ := fileIdentity(gitInfo)
	indexDigest, indexSize, err := hashRegularFile(filepath.Join(repository, ".git", "index"), int64(e.outputBytes))
	if err != nil || rootDevice != expected.rootDevice || rootInode != expected.rootInode || gitDevice != expected.gitDevice || gitInode != expected.gitInode ||
		indexDigest != expected.indexDigest || indexSize != expected.indexSize {
		return fmt.Errorf("%w: source identity changed", ErrUnsafeSource)
	}
	head, err := e.gitOutput(ctx, repository, nil, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || string(head) != string(expected.head)+"\n" {
		return fmt.Errorf("%w: HEAD changed", ErrUnsafeSource)
	}
	if err := e.proveArtifactRefsEmpty(ctx, repository); err != nil {
		return err
	}
	return nil
}

func (e *Engine) proveArtifactRefsEmpty(ctx context.Context, repository string) error {
	output, err := e.gitOutput(ctx, repository, nil, nil, "for-each-ref", "--format=%(refname)", "refs/fern-artifact")
	if err != nil || len(output) != 0 {
		return fmt.Errorf("%w: reserved artifact refs", ErrUnsafeSource)
	}
	return nil
}

func (e *Engine) captureTree(ctx context.Context, repository, stage, name string) (task.GitOID, error) {
	index := filepath.Join(stage, name)
	file, err := os.OpenFile(index, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(index); err != nil {
		return "", err
	}
	environment := []string{"GIT_INDEX_FILE=" + index}
	if _, err := e.gitOutput(ctx, repository, environment, nil, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if err := os.Chmod(index, 0o600); err != nil {
		return "", err
	}
	return e.refreshCapturedTree(ctx, repository, index)
}

func (e *Engine) refreshCapturedTree(ctx context.Context, repository, index string) (task.GitOID, error) {
	environment := []string{"GIT_INDEX_FILE=" + index}
	if _, err := e.gitOutput(ctx, repository, environment, nil, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := e.gitOutput(ctx, repository, environment, nil, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	if err := os.Chmod(index, 0o600); err != nil {
		return "", err
	}
	paths, err := e.gitOutput(ctx, repository, environment, nil, "ls-files", "-z")
	if err != nil {
		return "", err
	}
	attributes, err := e.gitOutput(ctx, repository, environment, bytes.NewReader(paths), "check-attr", "--all", "-z", "--stdin")
	if err != nil {
		return "", err
	}
	if err := rejectTransformAttributes(attributes); err != nil {
		return "", err
	}
	tree, err := e.oid(ctx, repository, "", environment, "write-tree")
	if err != nil {
		return "", err
	}
	return tree, nil
}

func rejectTransformAttributes(output []byte) error {
	if len(output) == 0 {
		return nil
	}
	if output[len(output)-1] != 0 {
		return fmt.Errorf("%w: malformed attributes", ErrUnsafeSource)
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	if len(fields)%3 != 0 {
		return fmt.Errorf("%w: malformed attributes", ErrUnsafeSource)
	}
	for index := 0; index < len(fields); index += 3 {
		attribute, value := string(fields[index+1]), string(fields[index+2])
		if value != "unspecified" && (attribute == "filter" || attribute == "working-tree-encoding" || attribute == "text" || attribute == "eol" || attribute == "ident") {
			return fmt.Errorf("%w: content-transforming attribute", ErrUnsafeSource)
		}
	}
	return nil
}

func (e *Engine) commitTree(ctx context.Context, repository string, tree, base task.GitOID, epoch int64) (task.GitOID, error) {
	date := "@" + strconv.FormatInt(epoch, 10) + " +0000"
	environment := []string{
		"GIT_AUTHOR_NAME=Fern Artifact", "GIT_AUTHOR_EMAIL=artifact@fern.invalid", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=Fern Artifact", "GIT_COMMITTER_EMAIL=artifact@fern.invalid", "GIT_COMMITTER_DATE=" + date,
	}
	return e.oid(ctx, repository, "", environment, "commit-tree", string(tree), "-p", string(base), "-m", "Fern retained background run artifact")
}

func (e *Engine) proveTree(ctx context.Context, repository string, object task.GitOID) error {
	output, err := e.gitOutput(ctx, repository, nil, nil, "ls-tree", "-r", "-z", "--full-tree", string(object))
	if err != nil {
		return err
	}
	if len(output) > 0 && output[len(output)-1] != 0 {
		return fmt.Errorf("%w: malformed tree", ErrUnsafeSource)
	}
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return fmt.Errorf("%w: malformed tree", ErrUnsafeSource)
		}
		fields := bytes.Fields(record[:tab])
		path := record[tab+1:]
		if len(fields) != 3 || !safeArtifactPath(path) || string(fields[0]) == "160000" || string(fields[1]) != "blob" || !validMode(string(fields[0])) {
			return fmt.Errorf("%w: unsupported tree entry", ErrUnsafeSource)
		}
	}
	return nil
}

func (e *Engine) oid(ctx context.Context, repository, revision string, environment []string, arguments ...string) (task.GitOID, error) {
	if revision != "" {
		arguments = append(arguments, "rev-parse", "--verify", revision)
	}
	output, err := e.gitOutput(ctx, repository, environment, nil, arguments...)
	if err != nil {
		return "", err
	}
	if len(output) != 41 || output[40] != '\n' {
		return "", fmt.Errorf("%w: malformed object ID", ErrVerification)
	}
	oid, err := task.ParseGitOID(string(output[:40]))
	if err != nil {
		return "", fmt.Errorf("%w: malformed object ID", ErrVerification)
	}
	return oid, nil
}

func (e *Engine) createBundle(ctx context.Context, repository, destination string, base, result task.GitOID) (Digest, int64, error) {
	// Keep bundle refs in the engine-owned staging tree. Mutating source refs
	// would make a host crash observable in, and potentially block, the retained
	// run clone on recovery.
	bundleRepository := filepath.Join(filepath.Dir(destination), "bundle-repository.git")
	if err := os.Mkdir(bundleRepository, 0o700); err != nil {
		return Digest{}, 0, err
	}
	bundleDevice, bundleInode, err := directoryIdentity(bundleRepository)
	if err != nil {
		return Digest{}, 0, err
	}
	if _, err := e.gitOutput(ctx, repository, nil, nil, "init", "--bare", "--object-format=sha1", bundleRepository); err != nil {
		return Digest{}, 0, err
	}
	environment := []string{
		"GIT_DIR=" + bundleRepository,
		"GIT_OBJECT_DIRECTORY=" + filepath.Join(repository, ".git", "objects"),
	}
	zero := strings.Repeat("0", 40)
	if _, err := e.gitOutput(ctx, repository, environment, nil, "update-ref", bundleBaseRef, string(base), zero); err != nil {
		return Digest{}, 0, err
	}
	if _, err := e.gitOutput(ctx, repository, environment, nil, "update-ref", bundleResultRef, string(result), zero); err != nil {
		return Digest{}, 0, err
	}
	file, err := openPrivateExclusive(destination)
	if err != nil {
		return Digest{}, 0, err
	}
	writer := &boundedHashWriter{file: file, hash: sha256.New(), limit: e.bundleBytes}
	runErr := e.gitTo(ctx, repository, environment, nil, writer, "bundle", "create", "-", bundleBaseRef, bundleResultRef)
	syncErr := file.Sync()
	closeErr := file.Close()
	if runErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		return Digest{}, 0, errors.Join(runErr, syncErr, closeErr)
	}
	var digest Digest
	copy(digest.value[:], writer.hash.Sum(nil))
	if err := removeExactDirectory(bundleRepository, bundleDevice, bundleInode); err != nil {
		return Digest{}, 0, err
	}
	return digest, writer.size, nil
}

func (e *Engine) gitOutput(ctx context.Context, repository string, environment []string, stdin io.Reader, arguments ...string) ([]byte, error) {
	stdout := &boundedBuffer{limit: e.outputBytes}
	err := e.gitTo(ctx, repository, environment, stdin, stdout, arguments...)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func (e *Engine) gitTo(ctx context.Context, repository string, environment []string, stdin io.Reader, stdout io.Writer, arguments ...string) error {
	current, err := exactRegular(e.gitExecutable, true)
	if err != nil || !os.SameFile(e.gitFile, current) || current.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: Git executable changed", ErrInvalidConfig)
	}
	if _, err := exactDirectory(repository, false); err != nil {
		return fmt.Errorf("%w: repository path changed", ErrGitFailed)
	}
	commandContext, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	base := []string{"--no-pager", "--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false",
		"-c", "core.attributesFile=" + os.DevNull, "-c", "core.excludesFile=" + os.DevNull, "-c", "core.fileMode=true", "-c", "core.symlinks=true",
		"-c", "core.autocrlf=false", "-c", "diff.external=", "-c", "diff.renames=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"-c", "protocol.allow=never", "-C", repository}
	command := exec.CommandContext(commandContext, e.gitExecutable, append(base, arguments...)...)
	command.Dir = repository
	command.Env = append([]string{
		"GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_DISCOVERY_ACROSS_FILESYSTEM=0",
		"GIT_EXTERNAL_DIFF=", "GIT_LITERAL_PATHSPECS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0",
		"HOME=" + os.DevNull, "LANG=C", "LC_ALL=C", "PAGER=cat", "PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=" + os.DevNull,
	}, environment...)
	command.Stdin = stdin
	stderr := &boundedBuffer{limit: 16 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	configureProcessGroup(command)
	err = command.Run()
	if commandContext.Err() != nil {
		return ErrGitTimeout
	}
	stdoutOver := false
	if bounded, ok := stdout.(*boundedBuffer); ok {
		stdoutOver = bounded.over
	}
	if stdoutOver || stderr.over || errors.Is(err, errLimit) {
		return ErrOutputLimit
	}
	if err != nil {
		return ErrGitFailed
	}
	return nil
}

func validMode(mode string) bool { return mode == "100644" || mode == "100755" || mode == "120000" }

func safeArtifactPath(path []byte) bool {
	if !gitref.ValidPathBytes(path) || strings.EqualFold(string(path), ".gitmodules") {
		return false
	}
	for _, component := range bytes.Split(path, []byte{'/'}) {
		if strings.EqualFold(string(component), ".git") {
			return false
		}
	}
	return true
}

type rawChange struct {
	path             []byte
	kind             string
	oldMode, newMode string
	oldOID, newOID   task.GitOID
}

func (e *Engine) buildChanges(ctx context.Context, repository string, base, result task.GitOID) ([]ChangeEntry, error) {
	if base == result {
		return []ChangeEntry{}, nil
	}
	output, err := e.gitOutput(ctx, repository, nil, nil, "diff-tree", "--no-commit-id", "--raw", "-r", "-z", "--abbrev=40", "--no-renames", "--no-ext-diff", string(base), string(result), "--")
	if err != nil {
		return nil, err
	}
	raw, err := parseRawChanges(output, e.manifestFiles)
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("%w: manifest diff", ErrVerification)
	}
	sort.Slice(raw, func(i, j int) bool { return bytes.Compare(raw[i].path, raw[j].path) < 0 })
	for index := 1; index < len(raw); index++ {
		if bytes.Equal(raw[index-1].path, raw[index].path) {
			return nil, fmt.Errorf("%w: duplicate path", ErrVerification)
		}
	}
	sizes, err := e.blobSizes(ctx, repository, raw)
	if err != nil {
		return nil, err
	}
	changes := make([]ChangeEntry, 0, len(raw))
	for _, change := range raw {
		entry := ChangeEntry{PathBase64: base64.StdEncoding.EncodeToString(change.path), Kind: change.kind}
		if change.oldOID != "" {
			entry.Old = &FileVersion{Mode: change.oldMode, BlobOID: change.oldOID, Size: sizes[change.oldOID]}
		}
		if change.newOID != "" {
			entry.New = &FileVersion{Mode: change.newMode, BlobOID: change.newOID, Size: sizes[change.newOID]}
		}
		changes = append(changes, entry)
	}
	return changes, nil
}

func parseRawChanges(output []byte, limit int) ([]rawChange, error) {
	changes := make([]rawChange, 0)
	for len(output) > 0 {
		headerEnd := bytes.IndexByte(output, 0)
		if headerEnd < 0 {
			return nil, ErrVerification
		}
		header := output[:headerEnd]
		output = output[headerEnd+1:]
		pathEnd := bytes.IndexByte(output, 0)
		if pathEnd < 0 {
			return nil, ErrVerification
		}
		path := append([]byte(nil), output[:pathEnd]...)
		output = output[pathEnd+1:]
		fields := bytes.Fields(header)
		if len(fields) != 5 || len(fields[0]) != 7 || fields[0][0] != ':' || !safeArtifactPath(path) {
			return nil, ErrVerification
		}
		change := rawChange{path: path, oldMode: string(fields[0][1:]), newMode: string(fields[1])}
		oldOID, newOID, status := string(fields[2]), string(fields[3]), string(fields[4])
		switch status {
		case "A":
			if change.oldMode != "000000" || oldOID != strings.Repeat("0", 40) || !validMode(change.newMode) {
				return nil, ErrVerification
			}
			change.kind, change.newOID = "added", task.GitOID(newOID)
		case "D":
			if change.newMode != "000000" || newOID != strings.Repeat("0", 40) || !validMode(change.oldMode) {
				return nil, ErrVerification
			}
			change.kind, change.oldOID = "deleted", task.GitOID(oldOID)
		case "M", "T":
			if !validMode(change.oldMode) || !validMode(change.newMode) {
				return nil, ErrVerification
			}
			change.kind, change.oldOID, change.newOID = "modified", task.GitOID(oldOID), task.GitOID(newOID)
		default:
			return nil, ErrVerification
		}
		for _, oid := range []task.GitOID{change.oldOID, change.newOID} {
			if oid != "" {
				if _, err := task.ParseGitOID(string(oid)); err != nil {
					return nil, ErrVerification
				}
			}
		}
		changes = append(changes, change)
		if len(changes) > limit {
			return nil, ErrOutputLimit
		}
	}
	return changes, nil
}

func (e *Engine) blobSizes(ctx context.Context, repository string, changes []rawChange) (map[task.GitOID]int64, error) {
	set := make(map[task.GitOID]struct{})
	for _, change := range changes {
		if change.oldOID != "" {
			set[change.oldOID] = struct{}{}
		}
		if change.newOID != "" {
			set[change.newOID] = struct{}{}
		}
	}
	oids := make([]string, 0, len(set))
	for oid := range set {
		oids = append(oids, string(oid))
	}
	sort.Strings(oids)
	input := strings.NewReader(strings.Join(oids, "\n") + "\n")
	output, err := e.gitOutput(ctx, repository, nil, input, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(output, []byte{'\n'})
	if len(lines) != len(oids)+1 || len(lines[len(lines)-1]) != 0 {
		return nil, ErrVerification
	}
	sizes := make(map[task.GitOID]int64, len(oids))
	for index, line := range lines[:len(lines)-1] {
		fields := bytes.Fields(line)
		if len(fields) != 3 || string(fields[0]) != oids[index] || string(fields[1]) != "blob" {
			return nil, ErrVerification
		}
		size, err := strconv.ParseInt(string(fields[2]), 10, 64)
		if err != nil || size < 0 || size > e.blobBytes {
			return nil, ErrOutputLimit
		}
		sizes[task.GitOID(oids[index])] = size
	}
	return sizes, nil
}

func hashRegularFile(path string, limit int64) (Digest, int64, error) {
	file, info, err := openPrivateRead(path, 0, false)
	if err != nil {
		return Digest{}, 0, err
	}
	defer file.Close()
	if info.Size() < 0 || info.Size() > limit {
		return Digest{}, 0, ErrOutputLimit
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(file, limit+1))
	if err != nil || written != info.Size() {
		return Digest{}, 0, errors.Join(err, ErrStorage)
	}
	var digest Digest
	copy(digest.value[:], hasher.Sum(nil))
	return digest, written, nil
}
