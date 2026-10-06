package runapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nebler/fern/internal/task"
)

// GitBaseVerifier is the production BaseVerifier. It proves an exact object is
// a commit reachable from the configured checkout's HEAD or origin tracking
// refs, and performs no mutation.
type GitBaseVerifier struct {
	repository, git string
	timeout         time.Duration
}

func NewGitBaseVerifier(repository, git string, timeout time.Duration) (*GitBaseVerifier, error) {
	if !filepath.IsAbs(repository) || filepath.Clean(repository) != repository || !filepath.IsAbs(git) || filepath.Clean(git) != git || timeout <= 0 || timeout > time.Minute {
		return nil, errors.New("valid configured repository verifier is required")
	}
	repositoryInfo, repositoryErr := os.Stat(repository)
	gitInfo, gitErr := os.Stat(git)
	if repositoryErr != nil || !repositoryInfo.IsDir() || gitErr != nil || gitInfo.IsDir() || gitInfo.Mode()&0o111 == 0 {
		return nil, errors.New("configured repository and Git executable must exist")
	}
	return &GitBaseVerifier{repository: repository, git: git, timeout: timeout}, nil
}

func (v *GitBaseVerifier) Verify(parent context.Context, oid task.GitOID) error {
	ctx, cancel := context.WithTimeout(parent, v.timeout)
	defer cancel()
	objectType, err := v.command(ctx, "cat-file", "-t", string(oid))
	if err != nil || !bytes.Equal(objectType, []byte("commit\n")) {
		if err != nil {
			return err
		}
		return errors.New("base object is not exactly a commit")
	}
	output, err := v.output(ctx, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/")
	if err != nil {
		return err
	}
	refs := []string{"HEAD"}
	for _, ref := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(ref, "refs/remotes/origin/") && !strings.ContainsAny(ref, "\x00\r") {
			refs = append(refs, ref)
		}
	}
	for _, ref := range refs {
		if v.run(ctx, "merge-base", "--is-ancestor", string(oid), ref) == nil {
			return nil
		}
	}
	return errors.New("base commit is not reachable from an allowed ref")
}
func (v *GitBaseVerifier) run(ctx context.Context, args ...string) error {
	_, err := v.command(ctx, args...)
	return err
}
func (v *GitBaseVerifier) output(ctx context.Context, args ...string) (string, error) {
	value, err := v.command(ctx, args...)
	return string(value), err
}
func (v *GitBaseVerifier) command(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"--no-pager", "--no-replace-objects", "-C", v.repository}
	command := exec.CommandContext(ctx, v.git, append(base, args...)...)
	command.Env = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "HOME=/", "LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	var output bytes.Buffer
	command.Stdout = &limitedWriter{writer: &output, remaining: 64 << 10}
	command.Stderr = &limitedWriter{remaining: 64 << 10}
	err := command.Run()
	return output.Bytes(), err
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > w.remaining {
		value = value[:w.remaining]
	}
	w.remaining -= len(value)
	if w.writer != nil {
		_, _ = w.writer.Write(value)
	}
	return original, nil
}
