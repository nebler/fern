// Package commit makes git commits in a thread's worktree, on the node,
// for an agent whose box can't: a box gets its repository's git folder
// read-only, so that nothing it writes there (a hook, a config that names a
// program) runs in T3's server, which uses git in the same repository
// (architecture §13). The node's git runs here as T3's user, with hooks and
// everything else that would run a program switched off: the worktree's
// files are the box's.
//
// So is any repository in a folder of the worktree, which the box can make
// with git init, and whose config is the box's own. Git's everyday commands
// (add, status, commit with paths, diff against the worktree) start a git
// in such a folder once the index holds it as a submodule, and that git
// runs the filters its config names: the box's program, on the node
// (reproduced 2026-11-13: `git add --all` twice was enough). No setting
// turns that off; a .gitmodules in the worktree overrides the ones there
// are. So a commit is made with the commands that never look into a
// folder's own repository (diff-files told to ignore submodules, ls-files,
// update-index, write-tree, commit-tree, update-ref), and never puts one
// into the index, where T3's own git status would find it.
package commit

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// Skipped are the folders left out because they hold a git repository
	// of their own, each ending in "/".
	Skipped []string
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

// Commit commits req's paths as the worktree has them: every change in
// them, new files that aren't ignored and files that are gone. Changes
// staged before, outside the paths, stay staged and uncommitted.
func (c Committer) Commit(ctx context.Context, req Request) (Result, error) {
	dir, paths := req.Dir, req.Paths
	if strings.TrimSpace(req.Message) == "" {
		return Result{}, errors.New("a commit needs a message")
	}
	// Reading where the repository is runs nothing its config names.
	out, err := c.git(ctx, dir, nil, nil, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir", "--git-dir")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Result{}, ErrNoGit
		}
		return Result{}, fmt.Errorf("%w: %s", ErrNotWorktree, dir)
	}
	where := strings.Split(strings.TrimSpace(out), "\n")
	if len(where) != 3 || where[0] != filepath.Clean(dir) || where[1] != filepath.Clean(req.GitDir) {
		return Result{}, fmt.Errorf("%w: %s", ErrNotWorktree, dir)
	}
	for _, key := range []string{"user.name", "user.email"} {
		if v, err := c.git(ctx, dir, nil, nil, "config", "--get", key); err != nil || strings.TrimSpace(v) == "" {
			return Result{}, ErrNoAuthor
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	// A repository without a commit yet has no HEAD to start from.
	head := ""
	if out, err := c.git(ctx, dir, nil, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		head = strings.TrimSpace(out)
	}
	// The worktree's own index first, so that it agrees with the commit
	// once it is made; then the commit's tree in an index of its own, which
	// starts as HEAD and takes the paths alone. It is in the worktree's git
	// folder, which the box can't write.
	skipped, err := c.stage(ctx, dir, nil, head, paths)
	if err != nil {
		return Result{}, err
	}
	index := filepath.Join(where[2], "index.fern-"+rand.Text())
	own := []string{"GIT_INDEX_FILE=" + index}
	defer func() {
		_ = os.Remove(index)
		_ = os.Remove(index + ".lock")
	}()
	was := emptyTree
	if head != "" {
		if _, err := c.git(ctx, dir, own, nil, "read-tree", head); err != nil {
			return Result{}, err
		}
		if out, err = c.git(ctx, dir, nil, nil, "rev-parse", head+"^{tree}"); err != nil {
			return Result{}, err
		}
		was = strings.TrimSpace(out)
	}
	if _, err := c.stage(ctx, dir, own, head, paths); err != nil {
		return Result{}, err
	}
	if out, err = c.git(ctx, dir, own, nil, "write-tree"); err != nil {
		return Result{}, err
	}
	tree := strings.TrimSpace(out)
	if tree == was {
		return Result{}, ErrNothing
	}
	args := []string{"commit-tree", "--no-gpg-sign", tree}
	if head != "" {
		args = append(args, "-p", head)
	}
	if out, err = c.git(ctx, dir, nil, strings.NewReader(req.Message), args...); err != nil {
		return Result{}, err
	}
	made := strings.TrimSpace(out)
	// With the commit it replaces named: a HEAD that moved meanwhile stays.
	subject, _, _ := strings.Cut(strings.TrimSpace(req.Message), "\n")
	if _, err := c.git(ctx, dir, nil, nil, "update-ref", "-m", "commit: "+subject, "HEAD", made, head); err != nil {
		return Result{}, err
	}
	out, err = c.git(ctx, dir, nil, nil, "show", "--no-patch", "--format=%h%x00%s", "HEAD")
	if err != nil {
		return Result{}, err
	}
	hash, subject, _ := strings.Cut(strings.TrimSpace(out), "\x00")
	res := Result{Hash: hash, Subject: subject, Branch: "HEAD", Skipped: skipped}
	if branch, err := c.git(ctx, dir, nil, nil, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		res.Branch = strings.TrimSpace(branch)
	}
	files, err := c.git(ctx, dir, nil, nil, "diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
	if err != nil {
		return Result{}, err
	}
	res.Files = strings.Count(files, "\x00")
	return res, nil
}

// emptyTree is git's name for a tree with nothing in it.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// gitlink is the mode of an index entry that is another repository's
// commit: a submodule.
const gitlink = "160000"

// stage brings the index (the worktree's own, or the one env names) up to
// the worktree for paths, as git add --all would, without git add: changed
// and deleted files that git tracks, and new ones it doesn't ignore. A
// folder with a repository of its own is left out and returned. One that
// got into the index all the same (a file the box replaced with a
// repository between two of the commands) is put back to what head has.
func (c Committer) stage(ctx context.Context, dir string, env []string, head string, paths []string) (skipped []string, err error) {
	changed, err := c.git(ctx, dir, env, nil, append([]string{"diff-files", "--ignore-submodules=all", "--name-only", "-z", "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	added, err := c.git(ctx, dir, env, nil, append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range strings.SplitSeq(changed+added, "\x00") {
		switch {
		case name == "":
		case strings.HasSuffix(name, "/"):
			skipped = append(skipped, name)
		default:
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		in := strings.NewReader(strings.Join(names, "\x00") + "\x00")
		if _, err := c.git(ctx, dir, env, in, "update-index", "--add", "--remove", "-z", "--stdin"); err != nil {
			return nil, err
		}
	}
	staged, err := c.links(ctx, dir, env, "ls-files", "--stage", "-z")
	if err != nil || len(staged) == 0 {
		return skipped, err
	}
	var committed map[string]string
	if head != "" {
		if committed, err = c.links(ctx, dir, nil, "ls-tree", "-r", "-z", head); err != nil {
			return nil, err
		}
	}
	for path, id := range staged {
		was, had := committed[path]
		if had && was == id {
			continue // a submodule the repository has, as it was committed
		}
		undo := []string{"update-index", "--force-remove", "--", path}
		if had {
			undo = []string{"update-index", "--cacheinfo", gitlink + "," + was + "," + path}
		}
		if _, err := c.git(ctx, dir, env, nil, undo...); err != nil {
			return nil, err
		}
		if !slices.Contains(skipped, path+"/") {
			skipped = append(skipped, path+"/")
		}
	}
	slices.Sort(skipped)
	return skipped, nil
}

// links returns the submodules a listing names, by path, with the commit
// each is at. The listing is ls-files --stage -z ("<mode> <id> <stage>\t
// <path>") or ls-tree -r -z ("<mode> commit <id>\t<path>").
func (c Committer) links(ctx context.Context, dir string, env []string, args ...string) (map[string]string, error) {
	out, err := c.git(ctx, dir, env, nil, args...)
	if err != nil {
		return nil, err
	}
	links := make(map[string]string)
	for entry := range strings.SplitSeq(out, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasPrefix(meta, gitlink+" ") {
			continue
		}
		fields := strings.Fields(meta)
		id := fields[1]
		if id == "commit" && len(fields) > 2 {
			id = fields[2]
		}
		links[path] = id
	}
	return links, nil
}

// git runs git in dir with args and stdin, and returns what it printed. env
// adds to its environment, such as an index of its own.
func (c Committer) git(ctx context.Context, dir string, env []string, stdin *strings.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Git, append(append([]string{}, safe...), args...)...)
	cmd.Dir = dir
	// Nothing of the daemon's environment, and no repository above dir: a
	// folder that isn't a worktree must not commit to one that holds it.
	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + c.Home, "LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir),
	}, env...)
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
