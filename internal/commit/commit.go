// Package commit makes git commits in a thread's worktree, on the node,
// for an agent whose box can't: a box gets its repository's git folder
// read-only, so that nothing it writes there (a hook, a config that names a
// program) runs in T3's server, which uses git in the same repository
// (architecture §13). The node's git runs here as T3's user, with hooks and
// everything else that would run a program switched off: the worktree's
// files are the box's.
package commit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Errors a caller tells its agent about.
var (
	// ErrNothing means the worktree has no changes to commit, or none in
	// the paths asked for.
	ErrNothing = errors.New("nothing to commit")
	// ErrNoAuthor means git has no author name and e-mail on this node.
	ErrNoAuthor = errors.New("git has no author name and e-mail here")
	// ErrNoGit means the node has no git.
	ErrNoGit = errors.New("git isn't installed on this server")
	// ErrNotWorktree means the folder isn't the top of a worktree of the
	// repository it should belong to.
	ErrNotWorktree = errors.New("not a worktree of its repository")
)

// Committer runs git for commits.
type Committer struct {
	Git string // the git program, such as "git"
	// User is who git runs as: T3's user, who owns the repositories. nil
	// runs it as the caller.
	User *syscall.Credential
	// Home is git's home folder, T3's: its .gitconfig names the author.
	Home string
}

// Request is a commit to make.
type Request struct {
	Dir string // the worktree's top
	// GitDir is the git folder of the repository Dir must be a worktree of,
	// the one a box can't write: a repository the box made in its workspace
	// has a config of the box's own, which can name programs for git to run.
	GitDir  string
	Message string
	// Paths are git's pathspecs, relative to Dir; none means every change.
	Paths []string
}

// Result is a commit that was made.
type Result struct {
	Hash    string // abbreviated
	Subject string
	Branch  string // "HEAD" when detached
	Files   int    // files the commit changed
}

// safe switches off what would run a program or wait for a person: hooks
// (also a hooks folder the repository's config names inside the worktree),
// the file monitor, signing and maintenance in the background.
var safe = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "commit.gpgsign=false",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
}

// Commit stages req's paths in its worktree and commits them. Changes staged
// before, outside the paths, stay staged and uncommitted.
func (c Committer) Commit(ctx context.Context, req Request) (Result, error) {
	dir, paths := req.Dir, req.Paths
	if strings.TrimSpace(req.Message) == "" {
		return Result{}, errors.New("a commit needs a message")
	}
	// Reading where the repository is runs nothing its config names.
	out, err := c.git(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Result{}, ErrNoGit
		}
		return Result{}, fmt.Errorf("%w: %s", ErrNotWorktree, dir)
	}
	top, common, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if top != filepath.Clean(dir) || common != filepath.Clean(req.GitDir) {
		return Result{}, fmt.Errorf("%w: %s", ErrNotWorktree, dir)
	}
	for _, key := range []string{"user.name", "user.email"} {
		if v, err := c.git(ctx, dir, nil, "config", "--get", key); err != nil || strings.TrimSpace(v) == "" {
			return Result{}, ErrNoAuthor
		}
	}
	only := len(paths) > 0
	if !only {
		paths = []string{"."}
	}
	if _, err := c.git(ctx, dir, nil, append([]string{"add", "--all", "--"}, paths...)...); err != nil {
		return Result{}, err
	}
	// What is staged, in those paths: none is exit code 0.
	if _, err := c.git(ctx, dir, nil, append([]string{"diff", "--cached", "--quiet", "--"}, paths...)...); err == nil {
		return Result{}, ErrNothing
	}
	args := []string{"commit", "--quiet", "--no-verify", "--no-gpg-sign", "--file=-"}
	if only {
		args = append(append(args, "--only", "--"), paths...)
	}
	if _, err := c.git(ctx, dir, strings.NewReader(req.Message), args...); err != nil {
		return Result{}, err
	}
	out, err = c.git(ctx, dir, nil, "show", "--no-patch", "--format=%h%x00%s", "HEAD")
	if err != nil {
		return Result{}, err
	}
	hash, subject, _ := strings.Cut(strings.TrimSpace(out), "\x00")
	res := Result{Hash: hash, Subject: subject, Branch: "HEAD"}
	if branch, err := c.git(ctx, dir, nil, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		res.Branch = strings.TrimSpace(branch)
	}
	files, err := c.git(ctx, dir, nil, "diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
	if err != nil {
		return Result{}, err
	}
	res.Files = strings.Count(files, "\x00")
	return res, nil
}

// git runs git in dir with args and stdin, and returns what it printed.
func (c Committer) git(ctx context.Context, dir string, stdin *strings.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Git, append(append([]string{}, safe...), args...)...)
	cmd.Dir = dir
	// Nothing of the daemon's environment, and no repository above dir: a
	// folder that isn't a worktree must not commit to one that holds it.
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + c.Home, "LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir),
	}
	if c.User != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: c.User}
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.String(), nil
}
