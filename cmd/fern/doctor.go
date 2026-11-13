package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/nebler/fern/internal/envfile"
	"github.com/nebler/fern/internal/github"
)

// node is where doctor, logs and top look: the node's files, and T3's
// address.
type node struct {
	etc, pool, proc, bin string // /etc/fern, /var/lib/fern, /proc, /usr/local/bin
	sys                  string // /sys, where the kernel names a loop device's file and size
	poolImage            string // the pool's sparse file's folder, /var/lib/fern-img
	cgroup               string // the cgroup that holds a cgroup per box that is up, /sys/fs/cgroup/fern
	bundles              string // the bundles of the boxes that may run, /run/fern/boxes
	t3                   string // T3's server, http://127.0.0.1:30773
	git                  string // the node's git, which makes Claude's commits
	btrfs                string // btrfs-progs' btrfs
	losetup              string // util-linux's losetup, for fern pool grow
	journalctl           string // systemd's journalctl, for fern logs
	reboot               string // the file a Debian or Ubuntu system has while it asks for a reboot, /var/run/reboot-required
	github, cloudflare   string // GitHub's and Cloudflare's APIs
	// room returns the size and the free space of the filesystem that holds
	// a path, for fern pool grow.
	room func(path string) (total, free uint64, err error)
}

// thisNode returns the node doctor runs on.
func thisNode() node {
	return node{
		etc: "/etc/fern", pool: "/var/lib/fern", poolImage: "/var/lib/fern-img", proc: "/proc", sys: "/sys", bin: "/usr/local/bin",
		cgroup: "/sys/fs/cgroup/fern", bundles: "/run/fern/boxes",
		t3: "http://127.0.0.1:30773", git: "git", btrfs: "btrfs", losetup: "losetup", journalctl: "journalctl",
		github: github.API, cloudflare: "https://api.cloudflare.com/client/v4", room: statfsRoom,
		reboot: "/var/run/reboot-required",
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
	return report(std.out, thisNode().check(ctx, cl))
}

// report prints findings, one a line, each with what to do if it isn't ok,
// and returns 1 if one is broken.
func report(out io.Writer, findings []finding) (int, error) {
	code := 0
	for _, f := range findings {
		mark := "ok  "
		switch {
		case f.info:
			mark = "--  "
		case !f.ok:
			mark, code = "FAIL", 1
		}
		if _, err := fmt.Fprintf(out, "%s  %-14s %s\n", mark, f.name, f.detail); err != nil {
			return 1, err
		}
		if !f.ok && f.fix != "" {
			if _, err := fmt.Fprintf(out, "      %-14s → %s\n", "", f.fix); err != nil {
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
	daemonUp := err == nil
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
	add(n.checkPool())

	// The pool is a sparse file: what it can still hold is what the disk
	// under it has.
	if f, ok := n.checkDisk(); ok {
		add(f)
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

	// A reboot the system asks for, after an update of its kernel or
	// libraries.
	add(n.checkReboot(time.Now()))

	// Commits: the node's git makes Claude's (its box can't write to git),
	// as the author T3's git has.
	add(n.checkCommits(ctx))

	// What the daemon knows has gone wrong, which is otherwise a line in its
	// log: failed golden builds, backups that fail, a pool that takes no
	// more snapshots or has no room left for btrfs's records, a turn's
	// snapshot that failed. An older daemon doesn't say.
	var problems []api.Problem
	if daemonUp {
		ctxP, cancel := context.WithTimeout(ctx, 5*time.Second)
		problems, _ = cl.Problems(ctxP)
		cancel()
	}
	failing := func(name string) bool {
		return slices.ContainsFunc(problems, func(p api.Problem) bool { return p.Name == name })
	}

	// Backups, which are optional.
	switch _, err := os.Stat(filepath.Join(n.etc, "backup.env")); {
	case err != nil:
		// The owner's choice, so no problem: but said as what it would cost.
		add(finding{name: "backups", info: true, detail: n.unsaved(time.Now())})
	case !failing("backups"):
		add(finding{name: "backups", ok: true, detail: "set up: hourly, encrypted; fern backup makes one now"})
	}
	for _, p := range problems {
		add(ofProblem(p))
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
	vars, err := envfile.Read(filepath.Join(n.etc, "cloudflare.env"))
	switch token := vars["CLOUDFLARE_API_TOKEN"]; {
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

// ofProblem is the finding for something the daemon says is wrong.
func ofProblem(p api.Problem) finding {
	detail := p.Detail
	if !p.At.IsZero() {
		detail += " (" + p.At.Local().Format("2 Jan 15:04") + ")"
	}
	return finding{name: p.Name, detail: detail, fix: p.Fix}
}

// unsaved says what a server without backups would lose today: how many
// environments it has, golden builders aside (a golden is built again), and
// when the newest of them last changed. It reads the pool itself, so it
// says the same with the daemon down. An environment last changed when its
// workspace's folder or its folder of snapshots did: T3 has a snapshot
// taken at every turn.
func (n node) unsaved(now time.Time) string {
	const none = "none: if this server is lost, so is every thread made on it (README: Backups)"
	records, err := filepath.Glob(filepath.Join(n.pool, "envs", "*", "env.json"))
	if err != nil {
		return none
	}
	count := 0
	var newest time.Time
	for _, record := range records {
		dir := filepath.Dir(record)
		if strings.HasPrefix(filepath.Base(dir), "golden-") {
			continue
		}
		count++
		changed := []string{filepath.Join(dir, "snaps")}
		var rec struct {
			Workspace string `json:"workspace"`
		}
		if data, err := os.ReadFile(record); err == nil && json.Unmarshal(data, &rec) == nil && rec.Workspace != "" {
			changed = append(changed, rec.Workspace)
		}
		for _, path := range changed {
			if fi, err := os.Stat(path); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
	}
	switch {
	case count == 0:
		return none
	case newest.IsZero():
		return fmt.Sprintf("none: %d environments are on this server only: if it is lost, so are they (README: Backups)", count)
	}
	return fmt.Sprintf("none: %d environments are on this server only, the newest changed %s ago: if it is lost, so are they (README: Backups)",
		count, roughly(now.Sub(newest)))
}

// roughly is d for a sentence: minutes under two hours, hours under two
// days, then days.
func roughly(d time.Duration) string {
	switch d = max(d, 0); {
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// rebootLate is how long a reboot the system asks for may wait before
// doctor fails: the kernel and the libraries every box runs on stay the old
// ones until it.
const rebootLate = 14 * 24 * time.Hour

// checkReboot says whether the system asks for a reboot, as Debian and
// Ubuntu do with a file after an update that needs one (a kernel, libc),
// naming the packages in a file beside it. It is information until it has
// waited rebootLate, then a failure. The daemon doesn't list it: a
// notification that stands for two weeks would teach its reader to ignore
// them.
func (n node) checkReboot(now time.Time) finding {
	fi, err := os.Stat(n.reboot)
	if n.reboot == "" || err != nil {
		return finding{name: "reboot", ok: true, detail: "the system asks for none"}
	}
	waited := now.Sub(fi.ModTime())
	detail := fmt.Sprintf("the system asks for one since %s (%s)", fi.ModTime().Local().Format("2 Jan"), roughly(waited))
	if data, err := os.ReadFile(n.reboot + ".pkgs"); err == nil {
		pkgs := strings.Fields(string(data))
		slices.Sort(pkgs)
		pkgs = slices.Compact(pkgs)
		if more := len(pkgs) - 3; more > 0 {
			detail += fmt.Sprintf(", for %s and %d more", strings.Join(pkgs[:3], ", "), more)
		} else if len(pkgs) > 0 {
			detail += ", for " + strings.Join(pkgs, ", ")
		}
	}
	return finding{
		name: "reboot", info: waited <= rebootLate, detail: detail + ": until then the server runs the kernel and libraries from before their update",
		fix: "when no turn runs (`fern top` shows the boxes that are up): `systemctl reboot`. Boxes stop with it and wake at their next command, " +
			"the daemon and T3 start by themselves, and a turn that runs then is cut off",
	}
}

// checkPool says whether the pool is mounted, and how much it has free.
func (n node) checkPool() finding {
	var fs unix.Statfs_t
	switch err := unix.Statfs(n.pool, &fs); {
	case err != nil:
		return finding{name: "pool", detail: err.Error(), fix: again}
	case fs.Type != btrfsMagic:
		return finding{name: "pool", detail: n.pool + " isn't btrfs: the pool isn't mounted", fix: "mount " + n.pool + " (it is in /etc/fstab), then systemctl restart fern-daemon fern-t3"}
	}
	free := fs.Bavail * uint64(max(fs.Bsize, 0))
	return finding{
		name: "pool", ok: free >= minFree, detail: fmt.Sprintf("btrfs, %.1f GB free", float64(free)/1e9),
		fix: "delete threads you no longer need in T3, or grow the pool: `fern pool grow +10G`, if the server's disk has the room (`fern df`)",
	}
}

// checkDisk says how much the disk under the pool has free, where the pool
// is a file on it; false for a pool on a partition of its own.
func (n node) checkDisk() (finding, bool) {
	var fs unix.Statfs_t
	if err := unix.Statfs(n.poolImage, &fs); err != nil {
		return finding{}, false
	}
	free := fs.Bavail * uint64(max(fs.Bsize, 0))
	return finding{
		name: "disk", ok: free >= minFree, detail: fmt.Sprintf("%.1f GB free under the pool", float64(free)/1e9),
		fix: "free space on the server's disk: the pool is a file on it, and fills it as it grows",
	}, true
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
