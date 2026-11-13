package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/github"
)

// doctor finds each part that is there or missing on a node laid out in a
// temporary folder: the pool there isn't btrfs, and the rest is as an
// install leaves it, but with forwarding off and no GitHub App.
func TestDoctor(t *testing.T) {
	d := newFakeDaemon(t)
	root := t.TempDir()
	n := node{
		etc: filepath.Join(root, "etc"), pool: filepath.Join(root, "pool"), poolImage: filepath.Join(root, "none"),
		proc: filepath.Join(root, "proc"), bin: filepath.Join(root, "bin"),
	}
	for path, data := range map[string]string{
		"etc/claude.env":                  "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-x\n",
		"etc/cloudflare.env":              "CLOUDFLARE_API_TOKEN=cf-x\n",
		"pool/images/fern-box:dev/root/x": "",
		"proc/sys/net/ipv4/ip_forward":    "0\n",
		"proc/meminfo":                    "MemTotal: 8000000 kB\nMemAvailable:    2048000 kB\n",
		"bin/runsc":                       "#!/bin/sh\necho 'runsc version release-20261012'\n",
		// A git that knows an author's name, and no e-mail.
		"bin/git": "#!/bin/sh\ncase $5 in user.name) echo Ada ;; esac\n",
	} {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/t3/environment" {
			http.NotFound(w, r)
		}
	}))
	defer t3.Close()
	n.t3 = t3.URL
	// Cloudflare takes the token for its account but not for its user.
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cf-x" || r.URL.Path != "/accounts" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer cf.Close()
	n.cloudflare = cf.URL
	n.git = filepath.Join(root, "bin/git")

	got := make(map[string]string)
	for _, f := range n.check(t.Context(), api.NewClient(d.socket)) {
		switch {
		case f.info:
			got[f.name] = "info"
		case f.ok:
			got[f.name] = "ok"
		case f.fix == "":
			t.Errorf("%s fails with no fix: %s", f.name, f.detail)
		default:
			got[f.name] = "fail"
		}
	}
	want := map[string]string{
		"daemon": "ok", "T3": "ok", "Claude login": "ok", "pool": "fail", "gVisor": "ok", "box image": "ok",
		"forwarding": "fail", "firewall": "info", "memory": "ok", "GitHub App": "info", "backups": "info",
		"Cloudflare": "info", "commits": "info", "reboot": "ok",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
	}
}

// doctor lists what the daemon knows has gone wrong, each with what to do,
// and doesn't call backups that fail set up and well.
func TestDoctorProblems(t *testing.T) {
	d := newFakeDaemon(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "backup.env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	n := node{etc: root, pool: root, poolImage: root, proc: root, bin: root, t3: "http://127.0.0.1:1", git: "false", btrfs: "false"}
	for _, tt := range []struct {
		name     string
		problems []api.Problem
		want     map[string]string // the findings about backups and builds
	}{
		{name: "nothing wrong", want: map[string]string{"backups": "ok"}},
		{
			name: "a failed build and failing backups",
			problems: []api.Problem{
				{Name: "backups", Detail: "the last backup failed: no route to host", Fix: "fern backup"},
				{Name: "golden build", Detail: "golden-app-1234: .fern/setup exited with code 1", Fix: "mend the script"},
			},
			want: map[string]string{"backups": "fail: the last backup failed: no route to host", "golden build": "fail: golden-app-1234: .fern/setup exited with code 1"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d.mu.Lock()
			d.problems = tt.problems
			d.mu.Unlock()
			got := make(map[string]string)
			for _, f := range n.check(t.Context(), api.NewClient(d.socket)) {
				if f.name != "backups" && f.name != "golden build" {
					continue
				}
				switch {
				case f.ok:
					got[f.name] = "ok"
				case f.fix == "":
					t.Errorf("%s fails with no fix: %s", f.name, f.detail)
				default:
					got[f.name] = "fail: " + f.detail
				}
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("findings (-want +got):\n%s", diff)
			}
		})
	}
}

// doctor says which permissions an account hasn't given the GitHub App, and
// is content when each has given all Fern uses.
func TestDoctorGitHub(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	all := `"contents": "write", "pull_requests": "write", "workflows": "write", "secrets": "write"`
	for _, tc := range []struct {
		name, installations string
		wantOK              bool
		wantDetail          string
	}{
		{"every permission", `[{"account": {"login": "ada"}, "permissions": {` + all + `}}]`, true, "installed on 1 accounts, with every permission Fern uses"},
		{"one account lacks two", `[{"account": {"login": "ada"}, "permissions": {` + all + `}},
			{"account": {"login": "bo"}, "permissions": {"contents": "write", "pull_requests": "write"}}]`, false, "bo lacks Workflows, Secrets"},
		{"installed nowhere", `[]`, false, "installed on no account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.installations))
			}))
			defer gh.Close()
			app, err := github.New(7, keyPEM, gh.URL)
			if err != nil {
				t.Fatal(err)
			}
			got := checkGitHub(t.Context(), app)
			if got.ok != tc.wantOK || got.detail != tc.wantDetail || (!got.ok && got.fix == "") {
				t.Errorf("checkGitHub() = %+v, want ok %v, %q, and a fix unless ok", got, tc.wantOK, tc.wantDetail)
			}
		})
	}
}

// Without backups, doctor says what the server would lose today: how many
// environments, golden builders aside, and when the newest last changed.
func TestUnsaved(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	type env struct {
		id                string
		workspace, snaps  time.Duration // how long ago each changed; 0: there is none
		workspaceInRecord bool          // the record names the workspace
	}
	tests := []struct {
		name string
		envs []env
		want string
	}{
		{name: "no environments", want: "none: if this server is lost, so is every thread made on it (README: Backups)"},
		{
			name: "only a golden builder",
			envs: []env{{id: "golden-app-12345678", workspace: time.Hour, workspaceInRecord: true}},
			want: "none: if this server is lost, so is every thread made on it (README: Backups)",
		},
		{
			name: "threads, the newest by its last snapshot",
			envs: []env{
				{id: "t3-1", workspace: 30 * time.Hour, snaps: 12 * time.Minute, workspaceInRecord: true},
				{id: "t3-2", workspace: 3 * time.Hour, snaps: 5 * time.Hour, workspaceInRecord: true},
				{id: "golden-app-12345678", workspace: time.Minute, workspaceInRecord: true},
			},
			want: "none: 2 environments are on this server only, the newest changed 12 minutes ago: if it is lost, so are they (README: Backups)",
		},
		{
			name: "a thread by its workspace",
			envs: []env{{id: "t3-1", workspace: 3 * 24 * time.Hour, snaps: 9 * 24 * time.Hour, workspaceInRecord: true}},
			want: "none: 1 environments are on this server only, the newest changed 3 days ago: if it is lost, so are they (README: Backups)",
		},
		{
			name: "a record that names no workspace",
			envs: []env{{id: "t3-1"}},
			want: "none: 1 environments are on this server only: if it is lost, so are they (README: Backups)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := t.TempDir()
			for _, e := range tt.envs {
				dir := filepath.Join(pool, "envs", e.id)
				workspace := filepath.Join(pool, "worktrees/app", e.id)
				record := "{"
				if e.workspaceInRecord {
					record = `{"version": 1, "workspace": "` + workspace + `"}`
				}
				for path, ago := range map[string]time.Duration{workspace: e.workspace, filepath.Join(dir, "snaps"): e.snaps, dir: time.Second} {
					if ago == 0 {
						continue
					}
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, now.Add(-ago), now.Add(-ago)); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, "env.json"), []byte(record), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := (node{pool: pool}).unsaved(now); got != tt.want {
				t.Errorf("unsaved() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A reboot the system asks for is information for two weeks, then a
// failure with what to do.
func TestCheckReboot(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		waited time.Duration // since the system asked; 0: it doesn't
		pkgs   string        // reboot-required.pkgs; "": none
		want   string        // ok, info or fail
		detail string        // a part of the detail
	}{
		{name: "none asked for", want: "ok", detail: "the system asks for none"},
		{name: "asked for three days ago", waited: 3 * 24 * time.Hour, pkgs: "linux-image-6.8.0-50-generic\nlibc6\nlinux-image-6.8.0-50-generic\n", want: "info", detail: "(3 days), for libc6, linux-image-6.8.0-50-generic:"},
		{name: "by many packages", waited: time.Hour, pkgs: "d\nc\nb\na\ne\n", want: "info", detail: "(60 minutes), for a, b, c and 2 more:"},
		{name: "without the packages' file", waited: 14 * 24 * time.Hour, want: "info", detail: "(14 days):"},
		{name: "asked for three weeks ago", waited: 21 * 24 * time.Hour, pkgs: "libc6\n", want: "fail", detail: "(21 days), for libc6:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := node{reboot: filepath.Join(t.TempDir(), "reboot-required")}
			if tt.waited != 0 {
				if err := os.WriteFile(n.reboot, []byte("*** System restart required ***\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(n.reboot, now.Add(-tt.waited), now.Add(-tt.waited)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.pkgs != "" {
				if err := os.WriteFile(n.reboot+".pkgs", []byte(tt.pkgs), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f := n.checkReboot(now)
			got := "fail"
			switch {
			case f.ok:
				got = "ok"
			case f.info:
				got = "info"
			}
			if got != tt.want || !strings.Contains(f.detail, tt.detail) || (!f.ok && f.fix == "") {
				t.Errorf("checkReboot() = %s %q with fix %q; want %s with %q in it, and a fix unless ok", got, f.detail, f.fix, tt.want, tt.detail)
			}
		})
	}
}
