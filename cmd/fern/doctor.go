package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
}

// thisNode returns the node doctor runs on.
func thisNode() node {
	return node{
		etc: "/etc/fern", pool: "/var/lib/fern", poolImage: "/var/lib/fern-img", proc: "/proc", bin: "/usr/local/bin",
		t3: "http://127.0.0.1:30773",
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

	// Backups, which are optional.
	if _, err := os.Stat(filepath.Join(n.etc, "backup.env")); err != nil {
		add(finding{name: "backups", info: true, detail: "none: if this server is lost, its threads are (README: backups)"})
	} else {
		add(finding{name: "backups", ok: true, detail: "set up: hourly, encrypted; fern backup makes one now"})
	}

	// The GitHub App, which is optional.
	switch app, err := github.Load(filepath.Join(n.etc, "github-app"), github.API); {
	case err != nil:
		add(finding{name: "GitHub App", detail: err.Error(), fix: "run the install again with FERN_GITHUB_APP_ID and FERN_GITHUB_APP_KEY (README)"})
	case app == nil:
		add(finding{name: "GitHub App", info: true, detail: "none: public repositories only, and no push or pull requests (README)"})
	default:
		add(finding{name: "GitHub App", ok: true, detail: "set up: T3's git and gh get tokens"})
	}
	return out
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
