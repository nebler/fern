package commit_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/commit"
)

// repo makes a repository with one commit and a home folder whose git
// config names an author, and returns the repository, the home folder and a
// function that runs git in the repository.
func repo(t *testing.T) (dir, home string, git func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	// As T3's worktrees: no symlink in its path.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = t.TempDir()
	git = func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "--global", "user.name", "Ada")
	git("config", "--global", "user.email", "ada@example.com")
	write(t, dir, "a.txt", "a\n")
	write(t, dir, "b.txt", "b\n")
	git("add", ".")
	git("commit", "-qm", "first")
	return dir, home, git
}

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

func TestCommit(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		want    commit.Result // without its hash
		wantErr error
		left    string // git status --short afterwards
		change  func(t *testing.T, dir string)
	}{
		{
			name:   "every change: a file changed, one removed, one new",
			change: func(t *testing.T, dir string) { edit(t, dir) },
			want:   commit.Result{Subject: "second", Branch: "main", Files: 3},
		},
		{
			name:   "the paths asked for, and nothing else",
			paths:  []string{"a.txt", "new/c.txt"},
			change: func(t *testing.T, dir string) { edit(t, dir) },
			want:   commit.Result{Subject: "second", Branch: "main", Files: 2},
			left:   "D b.txt",
		},
		{
			name:    "a worktree without changes",
			change:  func(*testing.T, string) {},
			wantErr: commit.ErrNothing,
		},
		{
			name:    "changes, but none in the paths asked for",
			paths:   []string{"b.txt"},
			change:  func(t *testing.T, dir string) { write(t, dir, "a.txt", "changed\n") },
			wantErr: commit.ErrNothing,
			left:    "M a.txt",
		},
		{
			name:   "a path that reads as an option is a path",
			paths:  []string{"--amend"},
			change: func(t *testing.T, dir string) { write(t, dir, "--amend", "x\n") },
			want:   commit.Result{Subject: "second", Branch: "main", Files: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, home, git := repo(t)
			tt.change(t, dir)
			got, err := commit.Committer{Git: "git", Home: home}.Commit(t.Context(), commit.Request{
				Dir: dir, GitDir: filepath.Join(dir, ".git"), Message: "second\n\nWhy it was done.\n", Paths: tt.paths,
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Commit() error = %v, want %v", err, tt.wantErr)
			}
			if err == nil {
				if want := git("rev-parse", "--short", "HEAD"); got.Hash != want {
					t.Errorf("Commit() hash = %q, want HEAD's, %q", got.Hash, want)
				}
				if got := git("log", "-1", "--format=%an <%ae>%n%B"); got != "Ada <ada@example.com>\nsecond\n\nWhy it was done." {
					t.Errorf("the commit reads %q", got)
				}
			}
			got.Hash = ""
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Commit() mismatch (-want +got):\n%s", diff)
			}
			if got := strings.TrimSpace(git("status", "--short")); got != tt.left {
				t.Errorf("git status afterwards = %q, want %q", got, tt.left)
			}
		})
	}
}

// edit changes a.txt, removes b.txt and adds new/c.txt.
func edit(t *testing.T, dir string) {
	t.Helper()
	write(t, dir, "a.txt", "changed\n")
	write(t, dir, "new/c.txt", "c\n")
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
}

// Nothing the worktree or the repository's config names runs: the worktree's
// files are the box's, and a hook would run as T3's user on the node.
func TestCommitRunsNoHooks(t *testing.T) {
	dir, home, git := repo(t)
	ran := filepath.Join(t.TempDir(), "ran")
	hook := "#!/bin/sh\necho hook >> " + ran + "\nexit 1\n"
	for _, name := range []string{"pre-commit", "commit-msg", "post-commit", "prepare-commit-msg"} {
		write(t, dir, filepath.Join(".git/hooks", name), hook)
		write(t, dir, filepath.Join("hooks", name), hook)
	}
	// As husky sets it: a hooks folder in the worktree. And signing, with a
	// program that would run.
	git("config", "core.hooksPath", "hooks")
	git("config", "commit.gpgsign", "true")
	git("config", "gpg.program", filepath.Join(dir, "hooks/pre-commit"))
	write(t, dir, "a.txt", "changed\n")
	req := commit.Request{Dir: dir, GitDir: filepath.Join(dir, ".git"), Message: "second"}
	if _, err := (commit.Committer{Git: "git", Home: home}).Commit(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(ran); err == nil {
		t.Errorf("a hook or the signing program ran: %q", data)
	}
}

func TestCommitRefuses(t *testing.T) {
	t.Run("a folder in a worktree, which isn't one", func(t *testing.T) {
		dir, home, _ := repo(t)
		write(t, dir, "sub/x.txt", "x\n")
		req := commit.Request{Dir: filepath.Join(dir, "sub"), GitDir: filepath.Join(dir, ".git"), Message: "second"}
		if _, err := (commit.Committer{Git: "git", Home: home}).Commit(t.Context(), req); !errors.Is(err, commit.ErrNotWorktree) {
			t.Errorf("Commit() error = %v, want %v", err, commit.ErrNotWorktree)
		}
	})
	// A repository the box made has a config of the box's own, whose filters
	// git would run: here one that leaves a mark.
	t.Run("a repository the box made in its workspace", func(t *testing.T) {
		dir, home, git := repo(t)
		ran := filepath.Join(t.TempDir(), "ran")
		sub := filepath.Join(dir, "sub")
		write(t, dir, "sub/x.txt", "x\n")
		write(t, dir, "sub/.gitattributes", "* filter=mark\n")
		git("init", "-q", sub)
		git("-C", sub, "config", "filter.mark.clean", "sh -c 'echo ran >> "+ran+"; cat'")
		req := commit.Request{Dir: sub, GitDir: filepath.Join(dir, ".git"), Message: "second"}
		if _, err := (commit.Committer{Git: "git", Home: home}).Commit(t.Context(), req); !errors.Is(err, commit.ErrNotWorktree) {
			t.Errorf("Commit() error = %v, want %v", err, commit.ErrNotWorktree)
		}
		if data, err := os.ReadFile(ran); err == nil {
			t.Errorf("the box's filter ran: %q", data)
		}
	})
	t.Run("a path outside the worktree", func(t *testing.T) {
		dir, home, git := repo(t)
		write(t, dir, "a.txt", "changed\n")
		req := commit.Request{Dir: dir, GitDir: filepath.Join(dir, ".git"), Message: "second", Paths: []string{"../elsewhere"}}
		_, err := commit.Committer{Git: "git", Home: home}.Commit(t.Context(), req)
		if err == nil || errors.Is(err, commit.ErrNothing) {
			t.Errorf("Commit() error = %v, want git's refusal", err)
		}
		if got := git("log", "--format=%s"); got != "first" {
			t.Errorf("the log reads %q, want only the first commit", got)
		}
	})
	t.Run("git without an author", func(t *testing.T) {
		dir, _, _ := repo(t)
		write(t, dir, "a.txt", "changed\n")
		req := commit.Request{Dir: dir, GitDir: filepath.Join(dir, ".git"), Message: "second"}
		if _, err := (commit.Committer{Git: "git", Home: t.TempDir()}).Commit(t.Context(), req); !errors.Is(err, commit.ErrNoAuthor) {
			t.Errorf("Commit() error = %v, want %v", err, commit.ErrNoAuthor)
		}
	})
	t.Run("a node without git", func(t *testing.T) {
		dir, home, _ := repo(t)
		req := commit.Request{Dir: dir, GitDir: filepath.Join(dir, ".git"), Message: "second"}
		if _, err := (commit.Committer{Git: "no-such-git", Home: home}).Commit(t.Context(), req); !errors.Is(err, commit.ErrNoGit) {
			t.Errorf("Commit() error = %v, want %v", err, commit.ErrNoGit)
		}
	})
	t.Run("no message", func(t *testing.T) {
		dir, home, _ := repo(t)
		write(t, dir, "a.txt", "changed\n")
		req := commit.Request{Dir: dir, GitDir: filepath.Join(dir, ".git"), Message: " \n"}
		if _, err := (commit.Committer{Git: "git", Home: home}).Commit(t.Context(), req); err == nil {
			t.Error("Commit() without a message returned no error")
		}
	})
}

// A T3 thread's folder is a linked worktree: the commit lands on its branch.
func TestCommitInALinkedWorktree(t *testing.T) {
	dir, home, git := repo(t)
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt = filepath.Join(wt, "t3-1")
	git("worktree", "add", "-q", "-b", "t3/one", wt)
	write(t, wt, "a.txt", "from the thread\n")
	got, err := commit.Committer{Git: "git", Home: home}.Commit(t.Context(), commit.Request{
		Dir: wt, GitDir: filepath.Join(dir, ".git"), Message: "thread work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "t3/one" || got.Files != 1 {
		t.Errorf("Commit() = %+v, want one file on t3/one", got)
	}
	if got := git("log", "-1", "--format=%s", "t3/one"); got != "thread work" {
		t.Errorf("t3/one's last commit = %q", got)
	}
	if got := git("log", "-1", "--format=%s", "main"); got != "first" {
		t.Errorf("main's last commit = %q, want it untouched", got)
	}
}

// A folder of the worktree with a repository of its own is the box's to
// make, config and all. Once git's index holds it as a submodule, git add,
// git status and git commit with paths start a git in it, which runs the
// filters its config names: the box's program, on the node. A .gitmodules
// in the worktree undoes the settings that would stop that. So no commit
// runs one, whichever way the folder came to be there, and none puts such a
// folder into the index, where T3's git status would find it.
func TestCommitNeverRunsANestedRepository(t *testing.T) {
	// nest makes a repository in dir/name with one commit.
	nest := func(t *testing.T, dir, name string, git func(args ...string) string) {
		t.Helper()
		sub := filepath.Join(dir, name)
		write(t, dir, name+"/f.txt", "f\n")
		git("init", "-q", "-b", "main", sub)
		git("-C", sub, "add", ".")
		git("-C", sub, "commit", "-qm", "theirs")
	}
	// arm gives the nested repository a filter that leaves a mark for every
	// file git reads there, changes a file in it so that git has a reason
	// to look, and asks through .gitmodules for
	// submodules to be looked into whatever git's settings say.
	arm := func(t *testing.T, dir, name, ran string, git func(args ...string) string) {
		t.Helper()
		sub := filepath.Join(dir, name)
		git("-C", sub, "config", "filter.mark.clean", "sh -c 'echo ran >> "+ran+"; cat'")
		git("-C", sub, "config", "core.fsmonitor", "sh -c 'echo ran >> "+ran+"'")
		write(t, sub, ".git/info/attributes", "* filter=mark\n")
		write(t, sub, "f.txt", "changed by the box\n")
		write(t, dir, ".gitmodules", "[submodule \""+name+"\"]\n\tpath = "+name+"\n\turl = ./"+name+"\n\tignore = none\n")
	}
	tests := []struct {
		name string
		// setup makes the nested repository before the commits under test.
		setup func(t *testing.T, dir, ran string, git func(args ...string) string)
		paths [][]string // the paths of each commit, in order; nil is every change
		// skipped is what the commits say they left out, between them.
		skipped []string
		// links is what the index holds as submodules afterwards.
		links string
	}{
		{
			name: "a repository the box made in a folder, committed twice",
			setup: func(t *testing.T, dir, ran string, git func(args ...string) string) {
				nest(t, dir, "vendor/x", git)
				arm(t, dir, "vendor/x", ran, git)
			},
			paths:   [][]string{nil, nil, {"vendor"}, {"."}},
			skipped: []string{"vendor/x/"},
		},
		{
			name: "a submodule the repository has, whose folder the box filled",
			setup: func(t *testing.T, dir, ran string, git func(args ...string) string) {
				nest(t, dir, "sub", git)
				// As the repository's own history has it, before the box.
				git("add", "sub")
				git("commit", "-qm", "a submodule")
				arm(t, dir, "sub", ran, git)
			},
			paths: [][]string{nil, {"sub"}, {"."}, nil},
			links: "sub",
		},
		{
			name: "a tracked file the box replaced with a repository",
			setup: func(t *testing.T, dir, ran string, git func(args ...string) string) {
				if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
					t.Fatal(err)
				}
				nest(t, dir, "b.txt", git)
				arm(t, dir, "b.txt", ran, git)
			},
			paths:   [][]string{nil, {"b.txt"}, nil},
			skipped: []string{"b.txt/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, home, git := repo(t)
			ran := filepath.Join(t.TempDir(), "ran")
			tt.setup(t, dir, ran, git)
			var skipped []string
			for i, paths := range tt.paths {
				// Something of the worktree's own to commit each time.
				write(t, dir, "a.txt", strings.Repeat("more\n", i+1))
				req := commit.Request{Dir: dir, GitDir: filepath.Join(dir, ".git"), Message: "next", Paths: paths}
				res, err := commit.Committer{Git: "git", Home: home}.Commit(t.Context(), req)
				if err != nil && !errors.Is(err, commit.ErrNothing) {
					t.Fatalf("Commit() %d with paths %v error: %v", i, paths, err)
				}
				for _, name := range res.Skipped {
					if !slices.Contains(skipped, name) {
						skipped = append(skipped, name)
					}
				}
				if data, err := os.ReadFile(ran); err == nil {
					t.Fatalf("after commit %d with paths %v, the nested repository's program had run: %q", i, paths, data)
				}
			}
			if diff := cmp.Diff(tt.skipped, skipped); diff != "" {
				t.Errorf("what the commits left out mismatch (-want +got):\n%s", diff)
			}
			var links []string
			for _, line := range strings.Split(git("ls-files", "--stage"), "\n") {
				if meta, path, ok := strings.Cut(line, "\t"); ok && strings.HasPrefix(meta, "160000 ") {
					links = append(links, path)
				}
			}
			if got := strings.Join(links, " "); got != tt.links {
				t.Errorf("the index holds the submodules %q, want %q", got, tt.links)
			}
			if got := git("show", "--format=", "--name-only", "HEAD"); strings.Contains(got, "f.txt") {
				t.Errorf("the last commit holds the nested repository's file: %q", got)
			}
			if got := git("log", "--format=%s", "-1"); got != "next" {
				t.Errorf("the last commit is %q, want one of these", got)
			}
		})
	}
}
