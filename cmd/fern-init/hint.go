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
	if len(h.services) > 0 {
		fmt.Fprintf(&b, "- The box runs this repository's services (services.yaml): %s. Use them directly; one that isn't running yet starts at its first connection.\n",
			strings.Join(slices.Sorted(maps.Keys(h.services)), ", "))
		fmt.Fprintln(&b, "- Their data lives in the worktree (git ignores it), so T3's diff doesn't show data changes: say what you changed.")
		if h.guessed {
			fmt.Fprintln(&b, "- The repository has no services.yaml: Fern guessed these from package.json (and docker-compose's Postgres, as DATABASE_URL). If one doesn't run right, write a services.yaml at the root to replace the guess; `cat .fern/tools/guess.toml` shows the Node and package manager it chose.")
		}
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
	fmt.Fprintln(&b, "- Don't commit or push: the repository's git folder is read-only here. The person commits from T3.")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
