package server_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nebler/fern/internal/commit"
)

// Claude commits its thread's changes with the commit tool: the daemon
// makes the commit in the thread's worktree, on its branch.
func TestCommitTool(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	worktree := filepath.Join(repo, "..", "t3-1")
	t.Cleanup(func() { _ = os.RemoveAll(worktree) })
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "--global", "user.name", "Ada")
	git("config", "--global", "user.email", "ada@example.com")
	git("commit", "-q", "--allow-empty", "-m", "first")
	git("worktree", "add", "-q", "-b", "t3/one", worktree)

	b := newDemoBox(t)
	url, api := b.serve(t)
	api.Git = &commit.Committer{Git: "git", Home: home}
	setEnv := func(gitDir string) {
		b.st.mu.Lock()
		defer b.st.mu.Unlock()
		env := b.st.envs["demo"]
		env.Workspace, env.Git = filepath.Clean(worktree), gitDir
		b.st.envs["demo"] = env
	}
	setEnv(filepath.Join(repo, ".git"))
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := call(t, s, "commit", map[string]any{"message": "thread work"})
	want := "Committed 1 file to t3/one as " + git("rev-parse", "--short", "t3/one") + ": thread work\nNot pushed: the person pushes from T3."
	if got != want {
		t.Errorf("commit answered %q, want %q", got, want)
	}
	if got := call(t, s, "commit", map[string]any{"message": "again"}); got != "Nothing to commit: the worktree has no changes." {
		t.Errorf("a second commit answered %q", got)
	}

	// A folder that is no worktree of a repository the box can't write.
	setEnv("")
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "commit", Arguments: map[string]any{"message": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !res.IsError || !strings.Contains(text, "isn't a worktree") {
		t.Errorf("commit outside a worktree answered %q (error: %v), want a refusal", text, res.IsError)
	}
}

// Without git for it on the node's side, Claude has no commit tool.
func TestCommitToolAbsent(t *testing.T) {
	url, _ := newDemoBox(t).serve(t)
	s, err := connect(t, url, boxToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	tools, err := s.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "commit" {
			t.Error("a server without Git offers commit")
		}
	}
}
