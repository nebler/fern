// Command fern-git is git where a box writes the files: for T3's server on
// the node, and for Claude in its own box. It is `git` on their PATH and,
// through GIT_EXEC_PATH, the git among git's own programs, so every git they
// start and every git that git starts comes through here. It runs the real
// git, but never lets it open a repository that a box could have written.
//
// A thread's worktree is the box's to write. Its own git folder is not: it
// lies in the repository's (repos/, projects/), which every box gets
// read-only, as it gets the worktree's .git file that names it. But a box
// can make a repository in any folder of its worktree, config and all, and
// git opens such a folder by itself: once the index holds it as a submodule
// (a repository with submodules, or after one `git add --all`), git status,
// diff, add, commit -a, stash and fetch each start a git in it, and that
// git runs the programs its config names. That was the box's program in
// T3's service, and in Claude's box beside Claude's login (found
// 2026-11-13). No setting of git's stops it.
//
// So in a thread's folder this never asks a file the box writes where the
// repository is. A first version did, in shell: it read the folder's .git
// as git does, almost, and a .git file that ended in CR LF, or one swapped
// between two looks, got past it (found the same day). The rule now:
//
//   - The git that git starts for a folder's own repository is told
//     GIT_DIR=.git or GIT_DIR=. by its parent: in a thread's folder it is
//     refused quietly, which the parent reads as "nothing changed there".
//   - Any other git in a thread's worktree is given its repository outright
//     (GIT_DIR, GIT_WORK_TREE): the one the worktree's own .git file names,
//     which must lie in repos/ or projects/. Git then looks nowhere.
//   - Options that name a repository or git's programs (--git-dir,
//     --work-tree, --bare, --exec-path=) are refused there, with the reason,
//     and so is any git in an environment's own folder (envs/), whose .git,
//     if it has one, is the box's.
//   - Anywhere else it is git as ever.
//
// The folder a call runs in is settled before git starts: -C is applied
// here, by changing into it, so git can't be sent elsewhere between the look
// and the run.
package main

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// realGit is the git this stands in front of, in Debian's images. A test
// builds with another (-ldflags -X).
var realGit = "/usr/lib/git-core/git"

// defaultPool is where the pool is, unless FERN_POOL says otherwise, as
// T3's service has it.
const defaultPool = "/var/lib/fern"

func main() {
	pool := os.Getenv("FERN_POOL")
	if pool == "" {
		pool = defaultPool
	}
	err := run(os.Args[1:], os.Environ(), pool, realGit)
	var quiet quietly
	switch {
	case errors.As(err, &quiet):
		os.Exit(0)
	case err != nil:
		fmt.Fprintln(os.Stderr, "fern-git:", err)
		os.Exit(128) // as git's own "fatal"
	}
}

// quietly is the refusal of a git that git started in a folder's own
// repository: no word and no error, so the parent goes on.
type quietly struct{}

func (quietly) Error() string { return "refused quietly" }

// run starts the real git for args, as decide allows it, and doesn't return
// if it started; it returns why it didn't.
func run(args, environ []string, pool, git string) error {
	rest, explicit, err := enter(args)
	if err != nil {
		return err
	}
	// The kernel's own name for the folder this runs in, which has no
	// symlink in it and can't be led elsewhere: not $PWD, and not a path
	// walked again.
	here, err := syscall.Getwd()
	if err != nil {
		return fmt.Errorf("the folder git runs in: %w", err)
	}
	pool, err = physical(pool)
	if err != nil {
		// No pool here: nothing is a thread's folder.
		pool = ""
	}
	how, err := decide(here, pool, value(environ, "GIT_DIR"), explicit)
	if err != nil {
		return err
	}
	if how.gitDir != "" {
		environ = set(environ, "GIT_DIR", how.gitDir)
		environ = set(environ, "GIT_WORK_TREE", how.workTree)
	}
	if err := syscall.Exec(git, append([]string{"git"}, rest...), environ); err != nil {
		return fmt.Errorf("starting %s: %w", git, err)
	}
	return nil
}

// pinned is how git is started: with its repository and worktree given, or
// as it was called if both are empty.
type pinned struct{ gitDir, workTree string }

// decide says how a git called in folder here (resolved, as pool is) may
// run: as it is, with its repository given, or not at all. gitDir is the
// caller's GIT_DIR and explicit an option of the call that names a
// repository or git's programs, "" if none does.
func decide(here, pool, gitDir, explicit string) (pinned, error) {
	root, zone := threadFolder(here, pool)
	if zone == outside {
		return pinned{}, nil
	}
	// Git's own git for a folder's repository. At the worktree's top,
	// GIT_DIR=.git is the worktree's own, read below.
	if gitDir == "." || gitDir == ".git" && here != root {
		return pinned{}, quietly{}
	}
	if zone == environment {
		return pinned{}, fmt.Errorf("not opening a repository in %s: an environment's own folder is its box's to write, its .git with it", here)
	}
	if explicit != "" {
		return pinned{}, fmt.Errorf("%s isn't taken in a thread's folder (%s): git there opens the worktree's own repository and no other", explicit, here)
	}
	own, err := worktreeGitDir(root, pool)
	if err != nil {
		return pinned{}, fmt.Errorf("not opening a repository in %s: %w", here, err)
	}
	// A git that git started at the top, a hook's say, names the repository
	// it was started for: only the worktree's own.
	if gitDir != "" && gitDir != ".git" {
		named := gitDir
		if !filepath.IsAbs(named) {
			named = filepath.Join(here, named)
		}
		if got, err := physical(named); err != nil || got != own {
			return pinned{}, fmt.Errorf("not opening the repository GIT_DIR names (%s) in a thread's folder: it isn't the worktree's own", gitDir)
		}
	}
	return pinned{gitDir: own, workTree: root}, nil
}

// The kinds of folder a git can be called in.
const (
	outside     = iota // not a thread's: no box writes it
	worktree           // in a T3 worktree, pool/worktrees/<repository>/<name>
	environment        // in an environment's own folder, pool/envs/<id>
)

// threadFolder returns what kind of folder here is, and for one in a
// worktree the worktree's top.
func threadFolder(here, pool string) (root string, zone int) {
	if pool == "" {
		return "", outside
	}
	rel, err := filepath.Rel(pool, here)
	if err != nil || !filepath.IsLocal(rel) {
		return "", outside
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch {
	case parts[0] == "worktrees" && len(parts) >= 3:
		return filepath.Join(pool, parts[0], parts[1], parts[2]), worktree
	case parts[0] == "envs" && len(parts) >= 2:
		return "", environment
	}
	return "", outside
}

// worktreeGitDir returns the git folder that the worktree at root belongs
// to: what its .git file names. Every box has that file read-only, so it is
// T3's word, but it is read strictly all the same: a regular file that holds
// "gitdir: ", an absolute path and at most a newline, and the path a folder
// in the pool's repos/ or projects/, reached through no symlink.
func worktreeGitDir(root, pool string) (string, error) {
	file := filepath.Join(root, ".git")
	fi, err := os.Lstat(file)
	if err != nil {
		return "", fmt.Errorf("this folder is no worktree of a repository on this server: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return "", errors.New("its .git is not the file `git worktree add` writes")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	path, ok := strings.CutPrefix(strings.TrimSuffix(string(data), "\n"), "gitdir: ")
	if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("its .git file doesn't name a git folder as `git worktree add` writes it")
	}
	resolved, err := physical(path)
	if err != nil || resolved != path {
		return "", errors.New("the git folder its .git file names is not a folder reached without a symlink")
	}
	for _, repositories := range []string{"repos", "projects"} {
		if rel, err := filepath.Rel(filepath.Join(pool, repositories), path); err == nil && filepath.IsLocal(rel) && rel != "." {
			return path, nil
		}
	}
	return "", errors.New("the git folder its .git file names is not in this server's repos/ or projects/")
}

// enter applies the -C options before git's command by changing into their
// folders, as git would, and returns the arguments without them, and the
// first option, if any, that names a repository or git's programs.
func enter(args []string) (rest []string, explicit string, err error) {
	// Options of git itself that take their value as the next argument.
	separate := []string{"-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--attr-source"}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		arg := args[0]
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--git-dir", "--work-tree", "--bare":
			explicit = cmp.Or(explicit, name)
		case "--exec-path":
			// Alone it only prints the path.
			if name != arg {
				explicit = cmp.Or(explicit, name)
			}
		}
		switch {
		case arg == "-C" && len(args) >= 2:
			if err := os.Chdir(args[1]); err != nil {
				return nil, "", fmt.Errorf("cannot change to %q: %w", args[1], err)
			}
			args = args[2:]
		case slices.Contains(separate, arg) && len(args) >= 2:
			rest = append(rest, arg, args[1])
			args = args[2:]
		default:
			rest = append(rest, arg)
			args = args[1:]
		}
	}
	return append(rest, args...), explicit, nil
}

// physical returns path with no symlink in it.
func physical(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// value returns variable name's last value in environ, or "".
func value(environ []string, name string) string {
	v := ""
	for _, kv := range environ {
		if rest, ok := strings.CutPrefix(kv, name+"="); ok {
			v = rest
		}
	}
	return v
}

// set returns environ with variable name set to v, once.
func set(environ []string, name, v string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return append(out, name+"="+v)
}
