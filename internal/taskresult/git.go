package taskresult

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nebler/fern/internal/task"
)

var errOutputLimit = errors.New("bounded output limit exceeded")

type rawChange struct {
	path             []byte
	kind             string
	oldMode, newMode string
	oldOID, newOID   task.GitOID
}

func (c *Collector) proveStaticSafety(ctx context.Context, repositoryPath string) error {
	if _, err := secureRepositoryPath(repositoryPath); err != nil {
		return err
	}
	gitDirectory := filepath.Join(repositoryPath, ".git")
	for _, relative := range []string{"shallow", "info/grafts", "objects/info/alternates", "objects/info/http-alternates"} {
		if _, err := os.Lstat(filepath.Join(gitDirectory, filepath.FromSlash(relative))); err == nil || !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: unsupported object source", ErrRepositoryProof)
		}
	}
	config, err := c.git(ctx, repositoryPath, c.outputBytes, nil, "config", "--local", "--null", "--list")
	if err != nil {
		return err
	}
	if err := validateLocalConfig(config); err != nil {
		return err
	}
	checks := []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "--show-toplevel"}, repositoryPath + "\n"},
		{[]string{"rev-parse", "--is-inside-work-tree"}, "true\n"},
		{[]string{"rev-parse", "--show-object-format"}, "sha1\n"},
		{[]string{"rev-parse", "--absolute-git-dir"}, gitDirectory + "\n"},
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, gitDirectory + "\n"},
		{[]string{"rev-parse", "--is-shallow-repository"}, "false\n"},
	}
	for _, check := range checks {
		output, err := c.git(ctx, repositoryPath, 4096, nil, check.args...)
		if err != nil {
			return err
		}
		if string(output) != check.want {
			return fmt.Errorf("%w: repository identity", ErrRepositoryProof)
		}
	}
	replacements, err := c.git(ctx, repositoryPath, c.outputBytes, nil, "for-each-ref", "--format=%(refname)", "refs/replace")
	if err != nil {
		return err
	}
	if len(replacements) != 0 {
		return fmt.Errorf("%w: replacement refs", ErrRepositoryProof)
	}
	return nil
}

func validateLocalConfig(output []byte) error {
	dangerous := func(key string) bool {
		key = strings.ToLower(key)
		return strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.") ||
			key == "core.worktree" || key == "extensions.worktreeconfig" || key == "core.fsmonitor" ||
			key == "core.hookspath" || key == "core.attributesfile" || key == "core.excludesfile" ||
			key == "core.alternaterefscommand" || key == "extensions.partialclone" ||
			key == "diff.external" || (strings.HasPrefix(key, "diff.") && strings.HasSuffix(key, ".command")) ||
			strings.HasPrefix(key, "filter.") || strings.HasPrefix(key, "url.") && (strings.HasSuffix(key, ".insteadof") || strings.HasSuffix(key, ".pushinsteadof")) ||
			strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".promisor") || strings.HasSuffix(key, ".partialclonefilter"))
	}
	if len(output) > 0 && output[len(output)-1] != 0 {
		return fmt.Errorf("%w: malformed local config", ErrRepositoryProof)
	}
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		separator := bytes.IndexByte(record, '\n')
		if separator <= 0 || dangerous(string(record[:separator])) {
			return fmt.Errorf("%w: unsafe local config", ErrRepositoryProof)
		}
	}
	return nil
}

func (c *Collector) proveState(ctx context.Context, repositoryPath string, base task.GitOID) (task.GitOID, task.GitOID, error) {
	headOutput, err := c.git(ctx, repositoryPath, 128, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", err
	}
	result, err := parseOIDLine(headOutput)
	if err != nil {
		return "", "", fmt.Errorf("%w: HEAD", ErrRepositoryProof)
	}
	for _, oid := range []task.GitOID{base, result} {
		output, err := c.git(ctx, repositoryPath, 32, nil, "cat-file", "-t", string(oid))
		if err != nil {
			return "", "", err
		}
		if string(output) != "commit\n" {
			return "", "", fmt.Errorf("%w: commit object", ErrRepositoryProof)
		}
	}
	if _, err := c.git(ctx, repositoryPath, 0, nil, "merge-base", "--is-ancestor", string(base), string(result)); err != nil {
		return "", "", fmt.Errorf("%w: base ancestry", ErrRepositoryProof)
	}
	treeOutput, err := c.git(ctx, repositoryPath, 128, nil, "rev-parse", "--verify", string(result)+"^{tree}")
	if err != nil {
		return "", "", err
	}
	tree, err := parseOIDLine(treeOutput)
	if err != nil {
		return "", "", fmt.Errorf("%w: result tree", ErrRepositoryProof)
	}
	if err := c.proveCommitTrees(ctx, repositoryPath, base, result); err != nil {
		return "", "", err
	}
	if err := c.proveHeadAndClean(ctx, repositoryPath, result); err != nil {
		return "", "", err
	}
	return result, tree, nil
}

func (c *Collector) proveCommitTrees(ctx context.Context, repositoryPath string, base, result task.GitOID) error {
	commits := []task.GitOID{base}
	if result != base {
		output, err := c.git(ctx, repositoryPath, c.outputBytes, nil, "rev-list", string(base)+".."+string(result))
		if err != nil {
			return err
		}
		if len(output) == 0 || output[len(output)-1] != '\n' {
			return fmt.Errorf("%w: malformed commit range", ErrRepositoryProof)
		}
		for _, line := range bytes.Split(output[:len(output)-1], []byte{'\n'}) {
			oid, err := task.ParseGitOID(string(line))
			if err != nil {
				return fmt.Errorf("%w: malformed commit range", ErrRepositoryProof)
			}
			commits = append(commits, oid)
		}
	}
	seen := make(map[task.GitOID]struct{}, len(commits))
	for _, commit := range commits {
		if _, exists := seen[commit]; exists {
			continue
		}
		seen[commit] = struct{}{}
		if err := c.proveTree(ctx, repositoryPath, commit); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) proveTree(ctx context.Context, repositoryPath string, commit task.GitOID) error {
	output, err := c.git(ctx, repositoryPath, c.outputBytes, nil, "ls-tree", "-r", "-z", "--full-tree", string(commit))
	if err != nil {
		return err
	}
	if len(output) > 0 && output[len(output)-1] != 0 {
		return fmt.Errorf("%w: malformed tree", ErrRepositoryProof)
	}
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		tab := bytes.IndexByte(record, '\t')
		if tab < 0 {
			return fmt.Errorf("%w: malformed tree entry", ErrRepositoryProof)
		}
		fields := bytes.Fields(record[:tab])
		path := record[tab+1:]
		if len(fields) != 3 || bytes.Equal(path, []byte(".gitmodules")) || string(fields[0]) == "160000" || string(fields[1]) == "commit" {
			return fmt.Errorf("%w: .gitmodules or gitlink", ErrRepositoryProof)
		}
		if mode := string(fields[0]); mode != "100644" && mode != "100755" && mode != "120000" {
			return fmt.Errorf("%w: unsupported tree mode", ErrRepositoryProof)
		}
	}
	return nil
}

func (c *Collector) proveHeadAndClean(ctx context.Context, repositoryPath string, expected task.GitOID) error {
	status, err := c.git(ctx, repositoryPath, 0, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		if errors.Is(err, ErrGitTimeout) {
			return err
		}
		return fmt.Errorf("%w: worktree status", ErrRepositoryProof)
	}
	if len(status) != 0 {
		return fmt.Errorf("%w: worktree is not clean", ErrRepositoryProof)
	}
	head, err := c.git(ctx, repositoryPath, 128, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if string(head) != string(expected)+"\n" {
		return fmt.Errorf("%w: HEAD changed", ErrRepositoryProof)
	}
	return nil
}

func parseOIDLine(output []byte) (task.GitOID, error) {
	if len(output) != 41 || output[40] != '\n' {
		return "", ErrRepositoryProof
	}
	return task.ParseGitOID(string(output[:40]))
}

func parseRawDiff(output []byte) ([]rawChange, error) {
	changes := make([]rawChange, 0)
	for len(output) > 0 {
		headerEnd := bytes.IndexByte(output, 0)
		if headerEnd < 0 {
			return nil, ErrRepositoryProof
		}
		header := output[:headerEnd]
		output = output[headerEnd+1:]
		pathEnd := bytes.IndexByte(output, 0)
		if pathEnd < 0 {
			return nil, ErrRepositoryProof
		}
		path := append([]byte(nil), output[:pathEnd]...)
		output = output[pathEnd+1:]
		fields := bytes.Fields(header)
		if len(fields) != 5 || len(fields[0]) != 7 || fields[0][0] != ':' || !validGitPath(path) {
			return nil, ErrRepositoryProof
		}
		oldMode, newMode := string(fields[0][1:]), string(fields[1])
		oldRaw, newRaw, status := string(fields[2]), string(fields[3]), string(fields[4])
		change := rawChange{path: path, oldMode: oldMode, newMode: newMode}
		switch status {
		case "A":
			if oldMode != "000000" || oldRaw != strings.Repeat("0", 40) || !validBlobMode(newMode) {
				return nil, ErrRepositoryProof
			}
			change.kind = "added"
			change.newOID = task.GitOID(newRaw)
		case "D":
			if newMode != "000000" || newRaw != strings.Repeat("0", 40) || !validBlobMode(oldMode) {
				return nil, ErrRepositoryProof
			}
			change.kind = "deleted"
			change.oldOID = task.GitOID(oldRaw)
		case "M", "T":
			if !validBlobMode(oldMode) || !validBlobMode(newMode) {
				return nil, ErrRepositoryProof
			}
			change.kind = "modified"
			change.oldOID, change.newOID = task.GitOID(oldRaw), task.GitOID(newRaw)
		default:
			return nil, ErrRepositoryProof
		}
		if change.oldOID != "" {
			if _, err := task.ParseGitOID(string(change.oldOID)); err != nil {
				return nil, err
			}
		}
		if change.newOID != "" {
			if _, err := task.ParseGitOID(string(change.newOID)); err != nil {
				return nil, err
			}
		}
		changes = append(changes, change)
		if len(changes) > MaxManifestFiles {
			return nil, ErrRepositoryProof
		}
	}
	return changes, nil
}

func validBlobMode(mode string) bool { return mode == "100644" || mode == "100755" || mode == "120000" }

func validGitPath(path []byte) bool {
	if len(path) == 0 || len(path) > 4096 || path[0] == '/' || path[len(path)-1] == '/' || bytes.IndexByte(path, 0) >= 0 {
		return false
	}
	for _, component := range bytes.Split(path, []byte{'/'}) {
		if len(component) == 0 || bytes.Equal(component, []byte(".")) || bytes.Equal(component, []byte("..")) {
			return false
		}
	}
	return true
}

func (c *Collector) blobSizes(ctx context.Context, repositoryPath string, changes []rawChange) (map[task.GitOID]int64, error) {
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
	var input bytes.Buffer
	for _, oid := range oids {
		input.WriteString(oid)
		input.WriteByte('\n')
	}
	output, err := c.git(ctx, repositoryPath, c.outputBytes, bytes.NewReader(input.Bytes()), "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(output, []byte{'\n'})
	if len(lines) != len(oids)+1 || len(lines[len(lines)-1]) != 0 {
		return nil, fmt.Errorf("%w: blob metadata", ErrRepositoryProof)
	}
	sizes := make(map[task.GitOID]int64, len(oids))
	for index, line := range lines[:len(lines)-1] {
		fields := bytes.Fields(line)
		if len(fields) != 3 || string(fields[0]) != oids[index] || string(fields[1]) != "blob" {
			return nil, fmt.Errorf("%w: blob metadata", ErrRepositoryProof)
		}
		size, err := strconv.ParseInt(string(fields[2]), 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%w: blob size", ErrRepositoryProof)
		}
		sizes[task.GitOID(oids[index])] = size
	}
	return sizes, nil
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
	over   bool
}

func (writer *boundedBuffer) Write(value []byte) (int, error) {
	if len(value) > writer.limit-writer.buffer.Len() {
		remaining := writer.limit - writer.buffer.Len()
		if remaining > 0 {
			_, _ = writer.buffer.Write(value[:remaining])
		}
		writer.over = true
		return len(value), errOutputLimit
	}
	return writer.buffer.Write(value)
}

func (c *Collector) git(ctx context.Context, repositoryPath string, outputLimit int, stdin io.Reader, arguments ...string) ([]byte, error) {
	current, err := os.Lstat(c.gitExecutable)
	if err != nil || c.gitFile == nil || !os.SameFile(c.gitFile, current) || !current.Mode().IsRegular() ||
		current.Mode()&os.ModeSymlink != 0 || current.Mode()&0111 == 0 || current.Mode().Perm()&0022 != 0 {
		return nil, ErrRepositoryProof
	}
	base := []string{"--no-pager", "--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.attributesFile=" + os.DevNull, "-c", "core.excludesFile=" + os.DevNull, "-c", "diff.external=", "-c", "diff.renames=false", "-c", "protocol.allow=never", "-C", repositoryPath}
	command := exec.CommandContext(ctx, c.gitExecutable, append(base, arguments...)...)
	command.Dir = repositoryPath
	command.Env = []string{
		"GIT_ATTR_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_DISCOVERY_ACROSS_FILESYSTEM=0",
		"GIT_EXTERNAL_DIFF=", "GIT_LITERAL_PATHSPECS=1", "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0",
		"HOME=" + os.DevNull, "LANG=C", "LC_ALL=C", "PAGER=cat", "PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=" + os.DevNull,
	}
	command.Stdin = stdin
	stdout := &boundedBuffer{limit: outputLimit}
	stderr := &boundedBuffer{limit: 16 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	configureProcessGroup(command)
	err = command.Run()
	if ctx.Err() != nil {
		return nil, ErrGitTimeout
	}
	if stdout.over || stderr.over || errors.Is(err, errOutputLimit) {
		return nil, ErrGitOutputLimit
	}
	if err != nil {
		return nil, ErrRepositoryProof
	}
	return append([]byte(nil), stdout.buffer.Bytes()...), nil
}
