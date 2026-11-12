package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/github"
)

// node is where doctor looks: the node's files, and T3's address.
type node struct {
	etc, pool, proc, bin string // /etc/fern, /var/lib/fern, /proc, /usr/local/bin
	poolImage            string // the pool's sparse file's folder, /var/lib/fern-img
	t3                   string // T3's server, http://127.0.0.1:30773
	git                  string // the node's git, which makes Claude's commits
	github, cloudflare   string // GitHub's and Cloudflare's APIs
}

// thisNode returns the node doctor runs on.
func thisNode() node {
	return node{
		etc: "/etc/fern", pool: "/var/lib/fern", poolImage: "/var/lib/fern-img", proc: "/proc", bin: "/usr/local/bin",
		t3: "http://127.0.0.1:30773", git: "git", github: github.API, cloudflare: "https://api.cloudflare.com/client/v4",
	}
}

// boxImage is the image the daemon runs boxes from.
const boxImage = "fern-box:dev"

// finding is one of doctor's checks: ok, or what is wrong and what to do.
type finding struct {
	name   string
	ok     bool
	detail string // what it found
	fix    string // what to do when it isn't ok
	info   bool   // not ok, but nothing is broken (an optional part)
}

// btrfsMagic is btrfs's f_type in statfs.
const btrfsMagic = 0x9123683e

// minFree is the free space below which new environments and goldens may
// fail.
const minFree = 5 << 30

// doctor checks the node Fern runs on and says what to do about each
// problem: fern doctor, as root on the node. It exits 1 if one is broken.
func doctor(ctx context.Context, _ command, cl *api.Client, std stdio) (int, error) {
	findings := thisNode().check(ctx, cl)
	code := 0
	for _, f := range findings {
		mark := "ok  "
		switch {
		case f.info:
			mark = "--  "
		case !f.ok:
			mark, code = "FAIL", 1
		}
		if _, err := fmt.Fprintf(std.out, "%s  %-14s %s\n", mark, f.name, f.detail); err != nil {
			return 1, err
		}
		if !f.ok && f.fix != "" {
			if _, err := fmt.Fprintf(std.out, "      %-14s → %s\n", "", f.fix); err != nil {
				return 1, err
			}
		}
	}
	return code, nil
}

// again is the fix for what the installer sets up.
const again = "run the install again (README: Try it); it repairs what it set up"

// check runs every check, in the order a person would fix them.
func (n node) check(ctx context.Context, cl *api.Client) []finding {
	var out []finding
	add := func(f finding) { out = append(out, f) }

	// The daemon.
	ctxD, cancel := context.WithTimeout(ctx, 5*time.Second)
	envs, err := cl.List(ctxD)
	cancel()
	if err != nil {
		add(finding{name: "daemon", detail: err.Error(), fix: "systemctl status fern-daemon; journalctl -u fern-daemon -n 50"})
	} else {
		add(finding{name: "daemon", ok: true, detail: fmt.Sprintf("answers; %d environments", len(envs))})
	}

	// T3's server.
	add(n.checkT3(ctx))

	// Claude's login.
	data, err := os.ReadFile(filepath.Join(n.etc, "claude.env"))
	switch {
	case err != nil:
		add(finding{name: "Claude login", detail: err.Error(), fix: "run the install again: it asks for the token from `claude setup-token`"})
	case !strings.Contains(string(data), "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat"):
		add(finding{name: "Claude login", detail: n.etc + "/claude.env holds no token from `claude setup-token`", fix: "delete it and run the install again"})
	default:
		add(finding{name: "Claude login", ok: true, detail: "stored, for the boxes and T3"})
	}

	// The pool and its free space.
	var fs unix.Statfs_t
	switch err := unix.Statfs(n.pool, &fs); {
	case err != nil:
		add(finding{name: "pool", detail: err.Error(), fix: again})
	case fs.Type != btrfsMagic:
		add(finding{name: "pool", detail: n.pool + " isn't btrfs: the pool isn't mounted", fix: "mount " + n.pool + " (it is in /etc/fstab), then systemctl restart fern-daemon fern-t3"})
	default:
		free := fs.Bavail * uint64(max(fs.Bsize, 0))
		f := finding{name: "pool", ok: free >= minFree, detail: fmt.Sprintf("btrfs, %.1f GB free", float64(free)/1e9)}
		f.fix = "delete threads you no longer need in T3, or grow the pool (btrfs filesystem resize)"
		add(f)
	}

	// The pool is a sparse file: what it can still hold is what the disk
	// under it has.
	if err := unix.Statfs(n.poolImage, &fs); err == nil {
		free := fs.Bavail * uint64(max(fs.Bsize, 0))
		add(finding{
			name: "disk", ok: free >= minFree, detail: fmt.Sprintf("%.1f GB free under the pool", float64(free)/1e9),
			fix: "free space on the server's disk: the pool is a file on it, and fills it as it grows",
		})
	}

	// gVisor and the box image.
	if out, err := exec.CommandContext(ctx, filepath.Join(n.bin, "runsc"), "--version").Output(); err != nil {
		add(finding{name: "gVisor", detail: "runsc doesn't run: " + err.Error(), fix: again})
	} else {
		v, _, _ := strings.Cut(string(out), "\n")
		add(finding{name: "gVisor", ok: true, detail: v})
	}
	if _, err := os.Stat(filepath.Join(n.pool, "images", boxImage, "root")); err != nil {
		add(finding{name: "box image", detail: boxImage + " isn't unpacked in the pool", fix: again})
	} else {
		add(finding{name: "box image", ok: true, detail: boxImage})
	}

	// The network: boxes reach the internet through the node, and only SSH
	// reaches the node from outside.
	if data, err := os.ReadFile(filepath.Join(n.proc, "sys/net/ipv4/ip_forward")); err != nil || strings.TrimSpace(string(data)) != "1" {
		add(finding{name: "forwarding", detail: "IP forwarding is off: boxes have no internet", fix: "sysctl -w net.ipv4.ip_forward=1 (the install sets it at boot)"})
	} else {
		add(finding{name: "forwarding", ok: true, detail: "on: boxes reach the internet"})
	}
	if _, err := os.Stat(filepath.Join(n.etc, "firewall.nft")); err != nil {
		add(finding{name: "firewall", info: true, detail: "Fern's isn't installed (FERN_FIREWALL=off): keep T3's port 30773 and 30780 from the internet yourself"})
	} else if err := exec.CommandContext(ctx, "nft", "list", "table", "inet", "fern_host").Run(); err != nil {
		add(finding{name: "firewall", detail: "installed but not loaded: T3's port may be public", fix: "systemctl restart fern-firewall"})
	} else {
		add(finding{name: "firewall", ok: true, detail: "only SSH from outside"})
	}

	// Memory.
	if avail, err := memAvailable(filepath.Join(n.proc, "meminfo")); err == nil {
		add(finding{
			name: "memory", ok: avail >= 512<<20, detail: fmt.Sprintf("%d MB available", avail>>20),
			fix: "fewer threads awake at once, or a server with more memory",
		})
	}

	// Commits: the node's git makes Claude's (its box can't write to git),
	// as the author T3's git has.
	add(n.checkCommits(ctx))

	// Backups, which are optional.
	if _, err := os.Stat(filepath.Join(n.etc, "backup.env")); err != nil {
		add(finding{name: "backups", info: true, detail: "none: if this server is lost, its threads are (README: backups)"})
	} else {
		add(finding{name: "backups", ok: true, detail: "set up: hourly, encrypted; fern backup makes one now"})
	}

	// The GitHub App, which is optional.
	switch app, err := github.Load(filepath.Join(n.etc, "github-app"), n.github); {
	case err != nil:
		add(finding{name: "GitHub App", detail: err.Error(), fix: "run the install again with FERN_GITHUB_APP_ID and FERN_GITHUB_APP_KEY (README)"})
	case app == nil:
		add(finding{name: "GitHub App", info: true, detail: "none: public repositories only, and no push or pull requests (README)"})
	default:
		add(checkGitHub(ctx, app))
	}
	// Cloudflare's token, which is optional too.
	switch token, err := fileVar(filepath.Join(n.etc, "cloudflare.env"), "CLOUDFLARE_API_TOKEN"); {
	case errors.Is(err, os.ErrNotExist):
		add(finding{name: "Cloudflare", info: true, detail: "no token: Claude can't set up deploys or use Cloudflare's MCP server (README)"})
	case err != nil:
		add(finding{name: "Cloudflare", detail: err.Error(), fix: "check " + n.etc + "/cloudflare.env"})
	case token == "":
		add(finding{name: "Cloudflare", detail: n.etc + "/cloudflare.env holds no CLOUDFLARE_API_TOKEN", fix: "delete it and run the install again with FERN_CLOUDFLARE_TOKEN_FILE"})
	default:
		add(n.checkCloudflare(ctx, token))
	}
	return out
}

// checkCommits says whether Claude's commit tool can commit: the node needs
// git, and T3's git config, in its home in the pool, an author. Without
// either, threads still work, and the person commits elsewhere.
func (n node) checkCommits(ctx context.Context) finding {
	if _, err := exec.LookPath(n.git); err != nil {
		return finding{name: "commits", info: true, detail: "the server has no git: Claude can't commit its work (apt-get install git, or run the install again)"}
	}
	config := filepath.Join(n.pool, "projects", ".gitconfig")
	author := func(key string) string {
		out, _ := exec.CommandContext(ctx, n.git, "config", "--file", config, "--get", key).Output()
		return strings.TrimSpace(string(out))
	}
	name, email := author("user.name"), author("user.email")
	if name == "" || email == "" {
		return finding{
			name: "commits", info: true,
			detail: "git has no author: Claude can't commit its work, nor T3 (run the install again from your computer, which copies yours)",
		}
	}
	return finding{name: "commits", ok: true, detail: fmt.Sprintf("Claude and T3 commit as %s <%s>", name, email)}
}

// checkGitHub asks GitHub what each account the App is installed on lets it
// do. A permission the App has but an account hasn't accepted is missing
// there, and shows only when a thread needs it: a push of a workflow, or
// cloudflare_connect.
func checkGitHub(ctx context.Context, app *github.App) finding {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	installations, err := app.Installations(ctx)
	if err != nil {
		return finding{
			name: "GitHub App", detail: "GitHub doesn't accept it: " + err.Error(),
			fix: "check the App's ID and key: run the install again with FERN_GITHUB_APP_ID and FERN_GITHUB_APP_KEY (README)",
		}
	}
	if len(installations) == 0 {
		return finding{name: "GitHub App", detail: "installed on no account", fix: "install it on your repositories: Install App, in its settings on GitHub"}
	}
	// Without Contents or Pull requests T3 can't push; without Workflows or
	// Secrets only deploys from a thread can't be set up.
	var lacking []string
	optional := true
	for _, i := range installations {
		if lacks := i.Lacks(); len(lacks) > 0 {
			lacking = append(lacking, fmt.Sprintf("%s lacks %s", i.Account, strings.Join(lacks, ", ")))
			optional = optional && !slices.Contains(lacks, "Contents") && !slices.Contains(lacks, "Pull requests")
		}
	}
	if len(lacking) > 0 {
		return finding{
			name: "GitHub App", info: optional, detail: strings.Join(lacking, "; "),
			fix: "set them to Read and write in the App's settings (Permissions & events), then accept the request at github.com/settings/installations",
		}
	}
	return finding{name: "GitHub App", ok: true, detail: fmt.Sprintf("installed on %d accounts, with every permission Fern uses", len(installations))}
}

// checkCloudflare asks Cloudflare's API what token may read. Cloudflare's
// MCP server reads the user and the accounts when a user's token connects,
// and refuses one that can't ("Insufficient scope"); a deploy needs
// neither, so a token that fails only that is not broken.
func (n node) checkCloudflare(ctx context.Context, token string) finding {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	get := func(path string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.cloudflare+path, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	accounts, err := get("/accounts?per_page=5")
	if err != nil {
		return finding{name: "Cloudflare", info: true, detail: "token set; Cloudflare's API didn't answer: " + err.Error()}
	}
	edit := "edit the token at dash.cloudflare.com/profile/api-tokens (its value stays): "
	switch {
	case accounts == http.StatusUnauthorized || accounts == http.StatusBadRequest:
		return finding{
			name: "Cloudflare", detail: "Cloudflare refuses the token: it is wrong, expired or deleted",
			fix: "make a new one (README) and run the install again with FERN_CLOUDFLARE_TOKEN_FILE",
		}
	case accounts != http.StatusOK:
		return finding{
			name: "Cloudflare", detail: fmt.Sprintf("the token can't read its account (HTTP %d)", accounts),
			fix: edit + "add Account → Account Settings → Read",
		}
	}
	if user, err := get("/user"); err == nil && user != http.StatusOK {
		return finding{
			name: "Cloudflare", info: true, detail: "the token deploys, but Cloudflare's MCP server refuses it: it can't read its user",
			fix: edit + "add User → User Details → Read",
		}
	}
	return finding{name: "Cloudflare", ok: true, detail: "token works: Claude sets up deploys and gets Cloudflare's MCP server in Cloudflare repositories"}
}

// fileVar returns the value of name in the env file, "" if it has none.
func fileVar(file, name string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+"="); ok {
			return strings.Trim(v, `"'`), nil
		}
	}
	return "", nil
}

// checkT3 asks T3's server whether it is up.
func (n node) checkT3(ctx context.Context) finding {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.t3+"/.well-known/t3/environment", nil)
	if err == nil {
		var resp *http.Response
		if resp, err = http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = errors.New(resp.Status)
			}
		}
	}
	if err != nil {
		return finding{name: "T3", detail: "doesn't answer: " + err.Error(), fix: "systemctl status fern-t3; journalctl -u fern-t3 -n 50"}
	}
	return finding{name: "T3", ok: true, detail: "answers at " + n.t3}
}

// memAvailable returns MemAvailable from a /proc/meminfo, in bytes.
func memAvailable(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb << 10, err
		}
	}
	return 0, errors.New("no MemAvailable")
}
