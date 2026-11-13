package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// write writes data to file name under dir, making its folders.
func write(t *testing.T, dir, name, data string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
		t.Fatal(err)
	}
}

// How a git is let run, by the folder it is called in, the GIT_DIR it was
// given and the options that name a repository: as it is, with the
// worktree's own repository given to it, refused without a word (git's own
// git for a folder's repository), or refused with the reason.
func TestDecide(t *testing.T) {
	pool, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(pool, "repos/app/.git/worktrees/one")
	top := filepath.Join(pool, "worktrees/app/one")
	for _, dir := range []string{own, top + "/src/nested", filepath.Join(pool, "envs/demo/live/sub"), filepath.Join(pool, "repos/app/.git/modules/lib"), filepath.Join(pool, "worktrees/app/plain/sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, top, ".git", "gitdir: "+own+"\n")
	const (
		plain  = "as it is"
		given  = "with the worktree's repository"
		quiet  = "refused quietly"
		refuse = "refused"
	)
	tests := []struct {
		name     string
		here     string
		gitDir   string
		explicit string
		want     string
	}{
		{name: "the main repository", here: filepath.Join(pool, "repos/app"), want: plain},
		{name: "a hook's git in the main repository", here: filepath.Join(pool, "repos/app"), gitDir: ".git", want: plain},
		{name: "git's git for a submodule git set up itself", here: filepath.Join(pool, "repos/app/.git/modules/lib"), gitDir: ".", want: plain},
		{name: "the folder that holds a repository's worktrees", here: filepath.Join(pool, "worktrees/app"), want: plain},
		{name: "outside the pool", here: "/", want: plain},
		{name: "a worktree's top", here: top, want: given},
		{name: "a folder in a worktree", here: top + "/src", want: given},
		{name: "a folder in a worktree with a .git of the box's", here: top + "/src/nested", want: given},
		{name: "a hook's git at the top, told .git", here: top, gitDir: ".git", want: given},
		{name: "a git that git started, told the worktree's own", here: top + "/src", gitDir: own, want: given},
		{name: "a git told another repository", here: top, gitDir: top + "/src/nested/.git", want: refuse},
		{name: "git's git for a submodule in a worktree", here: top + "/src/nested", gitDir: ".git", want: quiet},
		{name: "git's git for a submodule's fetch", here: top + "/src/nested", gitDir: ".", want: quiet},
		{name: "--git-dir in a worktree", here: top, explicit: "--git-dir", want: refuse},
		{name: "another place for git's programs", here: top + "/src", explicit: "--exec-path", want: refuse},
		{name: "an environment's own folder", here: filepath.Join(pool, "envs/demo/live"), want: refuse},
		{name: "git's git for a submodule there", here: filepath.Join(pool, "envs/demo/live/sub"), gitDir: ".git", want: quiet},
		{name: "a folder among the worktrees that is none", here: filepath.Join(pool, "worktrees/app/plain/sub"), want: refuse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			how, err := decide(tt.here, pool, tt.gitDir, tt.explicit)
			got := plain
			switch {
			case errors.As(err, new(quietly)):
				got = quiet
			case err != nil:
				got = refuse
			case how != (pinned{}):
				got = given
				if diff := cmp.Diff(pinned{gitDir: own, workTree: top}, how, cmp.AllowUnexported(pinned{})); diff != "" {
					t.Errorf("the repository given mismatch (-want +got):\n%s", diff)
				}
			}
			if got != tt.want {
				t.Errorf("decide(%s, GIT_DIR=%q, %q) = %s (%v), want %s", tt.here, tt.gitDir, tt.explicit, got, err, tt.want)
			}
		})
	}
}

// A worktree's .git file is T3's, and read-only in every box. It is read
// strictly all the same: what git would read otherwise than this does is
// not taken at all.
func TestWorktreeGitDir(t *testing.T) {
	pool, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(pool, "repos/app/.git/worktrees/one")
	elsewhere := filepath.Join(pool, "worktrees/app/two/mine")
	for _, dir := range []string{own, elsewhere} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(own, filepath.Join(pool, "repos/link")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		file string // the .git file; "" for none, "dir" for a folder, "link" for a symlink to the real one
		ok   bool
	}{
		{name: "as git worktree add writes it", file: "gitdir: " + own + "\n", ok: true},
		{name: "without the newline", file: "gitdir: " + own, ok: true},
		{name: "ending in CR LF, which git strips and a shell doesn't", file: "gitdir: " + own + "\r\n"},
		{name: "two lines, which git reads as one path", file: "gitdir: " + own + "\nx\n"},
		{name: "a NUL, where git stops reading", file: "gitdir: " + own + "\x00zzz\n"},
		{name: "a relative path", file: "gitdir: ../../repos/app/.git/worktrees/one\n"},
		{name: "a path with .. in it", file: "gitdir: " + pool + "/repos/app/../app/.git/worktrees/one\n"},
		{name: "a git folder in a thread's folder", file: "gitdir: " + elsewhere + "\n"},
		{name: "a git folder through a symlink", file: "gitdir: " + pool + "/repos/link\n"},
		{name: "the folder of repositories itself", file: "gitdir: " + pool + "/repos\n"},
		{name: "a folder that isn't there", file: "gitdir: " + pool + "/repos/none\n"},
		{name: "no gitdir", file: "something else\n"},
		{name: "a folder named .git", file: "dir"},
		{name: "a symlink named .git", file: "link"},
		{name: "none"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(pool, "worktrees/app", "w"+string(rune('a'+i)))
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			switch tt.file {
			case "":
			case "dir":
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "link":
				write(t, root, "real", "gitdir: "+own+"\n")
				if err := os.Symlink("real", filepath.Join(root, ".git")); err != nil {
					t.Fatal(err)
				}
			default:
				write(t, root, ".git", tt.file)
			}
			got, err := worktreeGitDir(root, pool)
			if (err == nil) != tt.ok || tt.ok && got != own {
				t.Errorf("worktreeGitDir() = %q, %v; want ok %t and %q", got, err, tt.ok, own)
			}
		})
	}
}

// -C is applied here, and the options that name a repository are noticed.
func TestEnter(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		args     []string
		rest     []string
		explicit string
		in       string // the folder it ends in, under dir
	}{
		{name: "a plain call", args: []string{"status", "--short"}, rest: []string{"status", "--short"}},
		{name: "-C twice, as git adds them up", args: []string{"-C", "a", "-C", "b", "status"}, rest: []string{"status"}, in: "a/b"},
		{name: "-C after another option", args: []string{"-c", "a.b=c", "-C", "a", "--no-pager", "log", "-C"}, rest: []string{"-c", "a.b=c", "--no-pager", "log", "-C"}, in: "a"},
		{name: "a repository named", args: []string{"--git-dir", "x", "--work-tree=y", "rev-list", "HEAD"}, rest: []string{"--git-dir", "x", "--work-tree=y", "rev-list", "HEAD"}, explicit: "--git-dir"},
		{name: "git's programs from elsewhere", args: []string{"--exec-path=/tmp/x", "status"}, rest: []string{"--exec-path=/tmp/x", "status"}, explicit: "--exec-path"},
		{name: "asking where git's programs are", args: []string{"--exec-path"}, rest: []string{"--exec-path"}},
		{name: "an option of the command that reads like one of git's", args: []string{"clone", "--bare", "x"}, rest: []string{"clone", "--bare", "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(dir)
			rest, explicit, err := enter(tt.args)
			if err != nil {
				t.Fatalf("enter(%q) error: %v", tt.args, err)
			}
			if diff := cmp.Diff(tt.rest, rest); diff != "" {
				t.Errorf("enter(%q) arguments mismatch (-want +got):\n%s", tt.args, diff)
			}
			if explicit != tt.explicit {
				t.Errorf("enter(%q) explicit = %q, want %q", tt.args, explicit, tt.explicit)
			}
			here, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(dir, tt.in); here != want {
				t.Errorf("enter(%q) ended in %s, want %s", tt.args, here, want)
			}
		})
	}
	t.Run("a folder that isn't there", func(t *testing.T) {
		t.Chdir(dir)
		if _, _, err := enter([]string{"-C", "none", "status"}); err == nil {
			t.Error("enter(-C none) worked, want it refused: git must not run somewhere else instead")
		}
	})
}

// The whole of it, with real git on a tree laid out as the pool: a
// repository, a worktree of it, and in the worktree a repository the box
// made and armed. Its config names a filter, an ssh command and a file
// monitor that each leave a mark, a file in it has changed, the worktree's
// index holds it as a submodule, and a .gitmodules asks git to look into it
// whatever its settings say. Plain git runs the mark in most of these
// commands; through fern-git none does, however the box dresses the
// folder's .git up, and git's own work in the worktree still gets done,
// hooks that use git included.
func TestNoGitInABoxesRepository(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	out, err := exec.CommandContext(t.Context(), git, "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	core := strings.TrimSpace(string(out))
	gitsGit := filepath.Join(core, "git")
	if _, err := os.Stat(gitsGit); err != nil {
		t.Skipf("git's own git isn't at %s", gitsGit)
	}
	// As the images lay it out: git's programs, with fern-git as the git.
	shim := filepath.Join(t.TempDir(), "core")
	if err := os.Mkdir(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	programs, err := os.ReadDir(core)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range programs {
		if p.Name() != "git" {
			if err := os.Symlink(filepath.Join(core, p.Name()), filepath.Join(shim, p.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-ldflags", "-X main.realGit="+gitsGit, "-o", filepath.Join(shim, "git"), ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fern-git: %v: %s", err, out)
	}

	type tree struct{ pool, repo, top, mark string }
	// run runs a command in dir: through fern-git as T3 does, or with the
	// plain git, as a box does and as T3 did.
	run := func(t *testing.T, tr tree, guarded bool, dir string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), git, args...)
		env := []string{
			"HOME=" + tr.pool, "GIT_CONFIG_NOSYSTEM=1", "FERN_POOL=" + tr.pool,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@fern", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@fern",
			"PATH=" + filepath.Dir(git) + ":/usr/bin:/bin",
		}
		if guarded {
			cmd = exec.CommandContext(t.Context(), filepath.Join(shim, "git"), args...)
			env = append(env, "GIT_EXEC_PATH="+shim, "PATH="+shim+":/usr/bin:/bin",
				"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit")
		}
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(t *testing.T, tr tree, dir string, args ...string) string {
		t.Helper()
		out, err := run(t, tr, false, dir, args...)
		if err != nil {
			t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
		}
		return out
	}
	// lay makes the pool: the repository with a hook that uses git, its
	// worktree, and in it the box's repository nested/, staged as a
	// submodule and then armed.
	lay := func(t *testing.T) tree {
		t.Helper()
		pool, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		tr := tree{pool: pool, repo: filepath.Join(pool, "repos/app"), top: filepath.Join(pool, "worktrees/app/one"), mark: filepath.Join(pool, "ran")}
		for _, dir := range []string{tr.repo, filepath.Dir(tr.top)} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		must(t, tr, tr.repo, "init", "-q", "-b", "main")
		write(t, tr.repo, "a.txt", "a\n")
		must(t, tr, tr.repo, "add", "a.txt")
		must(t, tr, tr.repo, "commit", "-qm", "first")
		write(t, tr.repo, ".git/hooks/pre-commit", "#!/bin/sh\ngit diff --cached --name-only > "+filepath.Join(pool, "hook-saw")+"\n")
		must(t, tr, tr.repo, "worktree", "add", "-q", "-b", "t3/one", tr.top)
		nested := filepath.Join(tr.top, "nested")
		must(t, tr, tr.top, "init", "-q", "-b", "main", nested)
		write(t, nested, "f", "f\n")
		must(t, tr, nested, "add", "f")
		must(t, tr, nested, "commit", "-qm", "theirs")
		// As T3's own staging puts it into the index.
		must(t, tr, tr.top, "add", "--all")
		must(t, tr, tr.top, "commit", "-q", "--no-verify", "-m", "with a folder that has a repository")
		leave := "sh -c 'echo ran >> " + tr.mark + "'"
		must(t, tr, nested, "config", "filter.mark.clean", "sh -c 'echo ran >> "+tr.mark+"; cat'")
		must(t, tr, nested, "config", "core.sshCommand", leave)
		must(t, tr, nested, "config", "core.fsmonitor", leave)
		must(t, tr, nested, "config", "remote.origin.url", "ssh://nowhere.invalid/x")
		write(t, nested, ".git/info/attributes", "* filter=mark\n")
		write(t, nested, "f", "changed by the box\n")
		write(t, tr.top, ".gitmodules", "[submodule \"nested\"]\n\tpath = nested\n\turl = ssh://nowhere.invalid/x\n\tignore = none\n\tfetchRecurseSubmodules = true\n")
		write(t, tr.top, "a.txt", "changed\n")
		return tr
	}
	ran := func(tr tree) bool { _, err := os.Stat(tr.mark); return err == nil }

	// The commands T3 runs in a worktree, and one in the box's folder itself.
	commands := []struct {
		name string
		dir  string // under the worktree's top
		args []string
	}{
		{name: "status", args: []string{"status", "--porcelain"}},
		{name: "diff against HEAD", args: []string{"diff", "HEAD", "--stat"}},
		{name: "add --all", args: []string{"add", "--all"}},
		{name: "commit -a", args: []string{"commit", "-qam", "next"}},
		{name: "stash", args: []string{"stash"}},
		{name: "fetch with submodules", args: []string{"fetch", "--recurse-submodules=yes", ".", "main"}},
		{name: "status with -C to the box's folder", args: []string{"-C", "nested", "status", "--porcelain"}},
		{name: "status in the box's folder", dir: "nested", args: []string{"status", "--porcelain"}},
		{name: "status in a folder of the worktree", dir: "nested/..", args: []string{"status", "--porcelain"}},
	}
	t.Run("plain git runs the box's program", func(t *testing.T) {
		n := 0
		for _, c := range commands {
			tr := lay(t)
			_, _ = run(t, tr, false, filepath.Join(tr.top, c.dir), c.args...)
			if ran(tr) {
				n++
			}
		}
		// The test of fern-git below means something only while this holds.
		if n < 5 {
			t.Errorf("plain git ran the box's program in %d of %d commands, want most: the tree is no longer armed", n, len(commands))
		}
	})
	// How the box dresses up nested/.git before T3's git comes by.
	dress := []struct {
		name string
		do   func(t *testing.T, tr tree)
	}{
		{name: "a plain folder", do: func(*testing.T, tree) {}},
		{name: "a file ending in CR LF", do: func(t *testing.T, tr tree) { gitfile(t, tr.top, "gitdir: g\r\n") }},
		{name: "a file of two lines", do: func(t *testing.T, tr tree) { gitfile(t, tr.top, "gitdir: g\nx\n") }},
		{name: "a file with a NUL", do: func(t *testing.T, tr tree) { gitfile(t, tr.top, "gitdir: g\x00zzz\n") }},
		{name: "a symlink to its own", do: func(t *testing.T, tr tree) {
			nested := filepath.Join(tr.top, "nested")
			if err := os.Rename(filepath.Join(nested, ".git"), filepath.Join(nested, "g")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("g", filepath.Join(nested, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, d := range dress {
		t.Run("fern-git, the folder's .git "+d.name, func(t *testing.T) {
			for _, c := range commands {
				tr := lay(t)
				d.do(t, tr)
				out, err := run(t, tr, true, filepath.Join(tr.top, c.dir), c.args...)
				if ran(tr) {
					t.Errorf("%s ran the box's program (%v): %s", c.name, err, out)
				}
			}
		})
	}
	t.Run("fern-git does git's work in the worktree", func(t *testing.T) {
		tr := lay(t)
		if out, err := run(t, tr, true, tr.top, "status", "--porcelain"); err != nil || !strings.Contains(out, "M a.txt") {
			t.Errorf("status = %q, %v; want the worktree's own change", out, err)
		}
		if out, err := run(t, tr, true, filepath.Join(tr.top, "nested"), "rev-parse", "--show-toplevel"); err != nil || out != tr.top {
			t.Errorf("rev-parse --show-toplevel in a folder = %q, %v; want the worktree's top %s", out, err, tr.top)
		}
		if out, err := run(t, tr, true, tr.top, "commit", "-qam", "through the hook"); err != nil {
			t.Fatalf("commit: %v: %s", err, out)
		}
		if saw, _ := os.ReadFile(filepath.Join(tr.pool, "hook-saw")); !strings.Contains(string(saw), "a.txt") {
			t.Errorf("the repository's hook saw %q staged, want a.txt: its git must work", saw)
		}
		if out, _ := run(t, tr, true, tr.top, "log", "-1", "--format=%s"); out != "through the hook" {
			t.Errorf("the last commit is %q, want the one just made", out)
		}
		if out, err := run(t, tr, true, tr.repo, "status", "--porcelain"); err != nil {
			t.Errorf("status in the main repository: %v: %s", err, out)
		}
		if out, err := run(t, tr, true, tr.top, "--git-dir", filepath.Join(tr.top, "nested/.git"), "rev-list", "-1", "HEAD"); err == nil || !strings.Contains(out, "isn't taken in a thread's folder") {
			t.Errorf("--git-dir to the box's repository = %q, %v; want it refused with the reason", out, err)
		}
		if ran(tr) {
			t.Error("the box's program ran")
		}
	})
}

// gitfile moves nested/.git to nested/g and writes data as nested/.git: a
// file that names the folder, in a way of the box's choosing.
func gitfile(t *testing.T, top, data string) {
	t.Helper()
	nested := filepath.Join(top, "nested")
	if err := os.Rename(filepath.Join(nested, ".git"), filepath.Join(nested, "g")); err != nil {
		t.Fatal(err)
	}
	write(t, nested, ".git", data)
}
