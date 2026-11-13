package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nebler/fern/internal/setup"
)

// hintMark starts the CLAUDE.md that fern-init writes, so it knows the file
// is its own to replace.
const hintMark = "<!-- written by Fern's fern-init; it replaces this file at every start -->"

// hint is what Claude is told about its environment.
type hint struct {
	services map[string]service
	guessed  bool // Fern guessed the services (setup.MakeGuess)
	// preview is where the box's ports can be opened (FERN_PREVIEW), with
	// {port} for the port; "" if they can't.
	preview    string
	cloudflare bool // the repository deploys to Cloudflare (setup.IsCloudflare)
	// setup is the repository's setup script, which a golden build runs
	// (setup.Script); "" if it has none.
	setup string
	// short is what the repository asks for its box and doesn't get, in
	// words (shortOf); "" if it gets what it asks for.
	short string
	// tools: the repository pins tools, or Fern guessed some, which the box
	// installs with mise when it starts (setup.ToolsKey).
	tools bool
	// unreadable is why services.yaml can't be read; "" if it can. The box
	// that runs Claude's commands doesn't start until it can.
	unreadable string
	// notes are the active skill packs' notes (packNotes).
	notes []string
}

// briefClaude writes Claude's hint into claudeDir and gives it the active
// skill packs, for the repository in workspace dir. A repository file it
// can't read leaves out what it would have said, not the rest.
func briefClaude(claudeDir, dir string, services map[string]service, guessed bool, unreadable string) error {
	h := hint{services: services, guessed: guessed, preview: os.Getenv("FERN_PREVIEW"), unreadable: unreadable}
	var errs []error
	cloudflare, err := setup.IsCloudflare(dir)
	errs = append(errs, err)
	h.cloudflare = cloudflare
	h.setup, err = setup.Script(dir)
	errs = append(errs, err)
	asked, err := setup.AskedLimits(dir)
	errs = append(errs, err)
	h.short = shortOf(asked, os.Getenv("FERN_BOX_LIMITS"))
	toolsKey, err := setup.ToolsKey(dir)
	errs = append(errs, err)
	h.tools = toolsKey != ""
	var detected []string
	if h.cloudflare {
		detected = append(detected, "cloudflare")
	}
	packs := activePacks(os.Getenv("FERN_SKILLS"), detected)
	h.notes, err = packNotes(skillPacks, packs)
	errs = append(errs, err, writeHint(claudeDir, h), linkSkills(claudeDir, skillPacks, packs))
	return errors.Join(errs...)
}

// shortOf returns what a repository asks for its box (services.yaml's
// limits) and doesn't get, given what the box has, as the daemon passes it
// to an agent box (FERN_BOX_LIMITS, CPUs:MiB:processes): such as "4 CPUs
// (the box has 2)". "" means it has what was asked, or the daemon didn't
// say.
func shortOf(asked setup.Limits, given string) string {
	var cpus float64
	var memory, pids int64
	if n, err := fmt.Sscanf(given, "%g:%d:%d", &cpus, &memory, &pids); err != nil || n != 3 {
		return ""
	}
	var short []string
	if asked.CPUs > cpus {
		short = append(short, fmt.Sprintf("%g CPUs (the box has %g)", asked.CPUs, cpus))
	}
	if asked.Memory > memory {
		short = append(short, fmt.Sprintf("%d MiB of memory (the box has %d)", asked.Memory, memory))
	}
	if asked.Pids > pids {
		short = append(short, fmt.Sprintf("%d processes (the box has %d)", asked.Pids, pids))
	}
	return strings.Join(short, ", ")
}

// writeHint tells Claude where it runs: the user-level CLAUDE.md in Claude's
// folder (CLAUDE_CONFIG_DIR), which is outside the workspace, so the hint is
// never in a snapshot, a diff or the repository. A CLAUDE.md that someone
// else wrote there is left alone.
func writeHint(claudeDir string, h hint) error {
	path := filepath.Join(claudeDir, "CLAUDE.md")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && !strings.HasPrefix(string(old), hintMark) {
		return nil
	}
	var b strings.Builder
	fmt.Fprintln(&b, hintMark)
	fmt.Fprintln(&b, "# You are in a Fern environment")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "- This thread's worktree is its own environment, in a sandboxed box. Other threads have their own copies; nothing you change here reaches them or the main branch's data.")
	if h.unreadable != "" {
		fmt.Fprintf(&b, "- **services.yaml can't be read, so the box that runs your Bash commands doesn't start: every Bash call fails until the file is right.** The fault: %s. Mend it with Read and Edit, which work without that box; the next Bash call starts the box with the file as it is then.\n", h.unreadable)
	}
	if len(h.services) > 0 {
		fmt.Fprintf(&b, "- The box runs this repository's services (services.yaml): %s. Use them directly; one that isn't running yet starts at its first connection.\n",
			strings.Join(slices.Sorted(maps.Keys(h.services)), ", "))
		fmt.Fprintln(&b, "- Their data lives in the worktree (git ignores it), so T3's diff doesn't show data changes: say what you changed.")
		if h.guessed {
			fmt.Fprintln(&b, "- The repository has no services.yaml: Fern guessed these from package.json (and docker-compose's Postgres, as DATABASE_URL). If one doesn't run right, write a services.yaml at the root to replace the guess; `cat .fern/tools/guess.toml` shows the Node and package manager it chose.")
		}
	}
	fmt.Fprintln(&b, "- Only the worktree lasts. The box your Bash commands run in stops a few seconds after a command returns, unless a background command is running, and starts again at the next one with an empty /tmp and no processes. Keep anything you need later in the worktree, in a folder git ignores (such as .fern/scratch with a .gitignore of `*`), never in /tmp. The home folder there is /tmp too: what an installer puts under `~` (rustup, `pip install --user`, `cargo install`, `npm install -g`) is gone at the next command, so point installers into the worktree.")
	fmt.Fprintln(&b, "- Read, Write and Edit run beside that box, not in it: they see the worktree as Bash does, and nothing else of it. A file Bash wrote to /tmp isn't there for Read; write it into the worktree.")
	if h.tools {
		fmt.Fprintln(&b, "- The box installs the tools this repository pins (mise) when it starts, which is quick once they are there. If one is missing (`command not found`), the install failed or ran out of time: run `mise install` with Bash, with run_in_background if it is slow, and read what it says.")
	}
	if h.short != "" {
		fmt.Fprintf(&b, "- services.yaml asks for more than this server gives a box: %s. Commands that need more will be slow or run out of memory. Say so to the person when it matters: they raise the most a box may have on the server, with --box-max-cpus, --box-max-memory (MiB) and --box-max-pids in FERN_DAEMON_ARGS in /etc/fern/daemon.env, then `systemctl restart fern-daemon`.\n", h.short)
	}
	if h.setup != "" {
		fmt.Fprintf(&b, "- This repository has a setup script, %s: Fern runs it once, into the snapshot every new thread starts from, so what it installs and builds is normally here already. If it isn't (the snapshot was still being built when this thread started, or the script failed there), run it yourself, with Bash's run_in_background if it takes long: it is safe to rerun. When you change it, say so: threads started before the next snapshot don't have the change.\n", h.setup)
	}
	fmt.Fprintln(&b, "- Start dev servers and other commands that must keep running (`npm run dev`) with Bash's run_in_background, not with `&` or `nohup`: what a command leaves running ends with it.")
	if h.preview != "" {
		fmt.Fprintf(&b, "- The person opens a server in this box at %s, such as %s for port 5173: give them that link when you start one. It answers once something listens on the port.\n",
			h.preview, strings.ReplaceAll(h.preview, "{port}", "5173"))
	}
	if h.cloudflare {
		fmt.Fprintln(&b, "- This repository deploys to Cloudflare. Its dev server (wrangler, or the framework's Cloudflare adapter) emulates D1, R2 and KV locally, in .wrangler/state in the worktree: use their bindings in wrangler's config, with no account and no token. That data is snapshotted with every turn, too.")
		fmt.Fprintln(&b, "- Deploys happen when the person pushes from T3, through GitHub Actions: main is the live site, other branches get preview URLs. To set that up, or when the person asks to put the site online, use the fern-deploy skill. Don't run `wrangler deploy` or `wrangler login`; this box has no Cloudflare credentials, by design.")
		fmt.Fprintln(&b, "- If you have the `cloudflare` MCP server's tools, they act on the person's real Cloudflare account: use them to create a D1 database or an R2 bucket for production, read a deploy's build logs, or check what is live. Ask before deleting or changing anything there.")
	}
	fmt.Fprintln(&b, "- Every turn is snapshotted. T3's \"Revert files too\" brings back the files and the services' data together, so changing data is safe.")
	fmt.Fprintln(&b, "- To commit, use the `commit` tool (mcp__fern__commit), not `git commit`: the repository's git folder is read-only in this box, so git can read (`git status`, `git diff`, `git log`) but not write. Commit when a piece of work is done and checked, with a message that says what and why. You can't push, fetch or change branches: the person pushes from T3.")
	for _, n := range h.notes {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, n)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
