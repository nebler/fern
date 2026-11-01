package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/api"
)

// doctor finds each part that is there or missing on a node laid out in a
// temporary folder: the pool there isn't btrfs, and the rest is as an
// install leaves it, but with forwarding off and no GitHub App.
func TestDoctor(t *testing.T) {
	d := newFakeDaemon(t)
	root := t.TempDir()
	n := node{etc: filepath.Join(root, "etc"), pool: filepath.Join(root, "pool"), poolImage: filepath.Join(root, "none"),
		proc: filepath.Join(root, "proc"), bin: filepath.Join(root, "bin")}
	for path, data := range map[string]string{
		"etc/claude.env":                  "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-x\n",
		"pool/images/fern-box:dev/root/x": "",
		"proc/sys/net/ipv4/ip_forward":    "0\n",
		"proc/meminfo":                    "MemTotal: 8000000 kB\nMemAvailable:    2048000 kB\n",
		"bin/runsc":                       "#!/bin/sh\necho 'runsc version release-20261012'\n",
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
		"forwarding": "fail", "firewall": "info", "memory": "ok", "GitHub App": "info",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
	}
}
