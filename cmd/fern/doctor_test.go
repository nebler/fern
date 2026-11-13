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
	"testing"

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
		// A pool with no room left for btrfs's records: 100 MiB of 1 GiB
		// left, and 1 MiB unallocated.
		"bin/btrfs": "#!/bin/sh\necho '    Device unallocated:		           1048576'\necho 'Metadata,DUP: Size:1073741824, Used:968884224 (90.23%)'\n",
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
	n.btrfs = filepath.Join(root, "bin/btrfs")

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
		"Cloudflare": "info", "commits": "info", "pool records": "fail",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
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

func TestMetadataRoom(t *testing.T) {
	const usage = `Overall:
    Device size:		       64424509440
    Device allocated:		       53686042624
    Device unallocated:		       10738466816
    Used:			       49719373824

Data,single: Size:51521781760, Used:48090607616 (93.34%)
   /dev/loop0	51521781760

Metadata,DUP: Size:1073741824, Used:781533184 (72.79%)
   /dev/loop0	2147483648
`
	unallocated, size, used, ok := metadataRoom(usage)
	if !ok || unallocated != 10738466816 || size != 1073741824 || used != 781533184 {
		t.Errorf("metadataRoom() = %d, %d, %d, %t; want 10738466816, 1073741824, 781533184, true", unallocated, size, used, ok)
	}
	if _, _, _, ok := metadataRoom("ERROR: not a btrfs filesystem\n"); ok {
		t.Error("metadataRoom() of an error message says it read the numbers")
	}
}
