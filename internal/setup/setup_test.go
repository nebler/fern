package setup_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/setup"
)

func write(t *testing.T, dir, name, data string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func key(t *testing.T, dir, image string) string {
	t.Helper()
	k, err := setup.Key(dir, image)
	if err != nil {
		t.Fatalf("Key(%s) error: %v", dir, err)
	}
	return k
}

// The key changes with what a golden build depends on, and only with that.
func TestKey(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "services.yaml", "services: {pg: {run: ./pg.sh}}\ngolden: {inputs: [seed.sql]}\n")
	write(t, dir, "seed.sql", "insert 1;")
	write(t, dir, "main.go", "package main")
	base := key(t, dir, "box:1")
	if !regexp.MustCompile(`^golden-[0-9a-f]{16}$`).MatchString(base) {
		t.Errorf("Key() = %q, want golden-<16 hex>", base)
	}
	write(t, dir, "main.go", "package main // edited")
	if got := key(t, dir, "box:1"); got != base {
		t.Errorf("Key() after editing main.go = %q, want %q: it isn't an input", got, base)
	}
	for name, change := range map[string]func(){
		"a golden input": func() { write(t, dir, "seed.sql", "insert 2;") },
		"a lockfile":     func() { write(t, dir, "package-lock.json", "{}") },
		"a setup script": func() { write(t, dir, ".agents/setup", "npm ci") },
		"pinned tools":   func() { write(t, dir, "mise.toml", "[tools]\nnode = \"22\"\n") },
		"the services": func() {
			write(t, dir, "services.yaml", "services: {pg: {run: ./pg2.sh}}\ngolden: {inputs: [seed.sql]}\n")
		},
		"the box image": func() {},
	} {
		before := key(t, dir, "box:1")
		change()
		image := "box:1"
		if name == "the box image" {
			image = "box:2"
		}
		if got := key(t, dir, image); got == before {
			t.Errorf("Key() didn't change with %s", name)
		}
	}
}

// A repository that only pins its tools gets a golden: it holds them.
func TestKeyTools(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".tool-versions", "node 22\n")
	key(t, dir, "box")
}

func TestToolsKey(t *testing.T) {
	dir := t.TempDir()
	if k, err := setup.ToolsKey(dir); k != "" || err != nil {
		t.Errorf("ToolsKey() without tool files = %q, %v; want \"\", nil", k, err)
	}
	write(t, dir, "mise.toml", "[tools]\nnode = \"22\"\n")
	write(t, dir, "package.json", "{}")
	k1, err := setup.ToolsKey(dir)
	if err != nil || k1 == "" {
		t.Fatalf("ToolsKey() = %q, %v; want a key", k1, err)
	}
	write(t, dir, "package.json", `{"name": "app"}`)
	if k, _ := setup.ToolsKey(dir); k != k1 {
		t.Errorf("ToolsKey() changed with package.json: it isn't a tool file")
	}
	write(t, dir, ".config/mise/config.toml", "[tools]\npnpm = \"10\"\n")
	if k, _ := setup.ToolsKey(dir); k == k1 {
		t.Errorf("ToolsKey() didn't change with a tool file")
	}
}

func TestKeyErrors(t *testing.T) {
	if _, err := setup.Key(t.TempDir(), "box"); !errors.Is(err, setup.ErrNoContract) {
		t.Errorf("Key() of a repository without a contract error = %v, want %v", err, setup.ErrNoContract)
	}

	outside := t.TempDir()
	write(t, outside, "secret", "x")
	for name, prepare := range map[string]func(dir string){
		"an input outside the repository": func(dir string) {
			write(t, dir, "services.yaml", "golden: {inputs: [../secret]}\n")
		},
		"an input that is a symlink out of it": func(dir string) {
			write(t, dir, "services.yaml", "golden: {inputs: [seed.sql]}\n")
			if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "seed.sql")); err != nil {
				t.Fatal(err)
			}
		},
		"an input in a directory that is a symlink out of it": func(dir string) {
			write(t, dir, "services.yaml", "golden: {inputs: [db/seed.sql]}\n")
			if err := os.Symlink(outside, filepath.Join(dir, "db")); err != nil {
				t.Fatal(err)
			}
		},
		"an input that is a pipe": func(dir string) {
			write(t, dir, "services.yaml", "golden: {inputs: [seed.sql]}\n")
			if err := syscall.Mkfifo(filepath.Join(dir, "seed.sql"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			prepare(dir)
			if _, err := setup.Key(dir, "box"); err == nil {
				t.Error("Key() error = nil, want one")
			}
		})
	}
}

func TestPreviewPort(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       int
		wantErr    bool
	}{
		{"no file", "", 0, false},
		{"no port", "services:\n  db:\n    run: postgres\n", 0, false},
		{"first by name", "services:\n  web:\n    run: a\n    port: 3000\n  api:\n    run: b\n    port: 4000\n", 4000, false},
		{"not a port", "services:\n  web:\n    run: a\n    port: 70000\n", 0, false},
		{"broken", "services: [", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.yaml != "" {
				if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(tc.yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := setup.PreviewPort(dir)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("PreviewPort = %d, %v; want %d, error %t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestAskedLimits(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       setup.Limits
		wantErr    bool
	}{
		{name: "no file"},
		{name: "no limits", yaml: "services:\n  db:\n    run: postgres\n"},
		{name: "all", yaml: "limits:\n  cpus: 1.5\n  memory: 4096\n  pids: 500\n", want: setup.Limits{CPUs: 1.5, Memory: 4096, Pids: 500}},
		{name: "one", yaml: "limits: {memory: 512}\n", want: setup.Limits{Memory: 512}},
		{name: "broken", yaml: "limits: [", wantErr: true},
		{name: "not a number", yaml: "limits: {memory: lots}\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.yaml != "" {
				if err := os.WriteFile(filepath.Join(dir, "services.yaml"), []byte(tc.yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := setup.AskedLimits(dir)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("AskedLimits = %+v, %v; want %+v, error %t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// A repository's setup script is .fern/setup, else .agents/setup, and only a
// regular file: the workspace is the agent's.
func TestScript(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		link  string // a symlink at this path, to a script outside
		want  string
	}{
		{name: "none"},
		{name: "Fern's", files: []string{".fern/setup"}, want: ".fern/setup"},
		{name: "Amp's and Delta's", files: []string{".agents/setup"}, want: ".agents/setup"},
		{name: "Fern's replaces the other", files: []string{".agents/setup", ".fern/setup"}, want: ".fern/setup"},
		{name: "a link isn't one", link: ".fern/setup"},
		{name: "a link, and a file in the other place", files: []string{".agents/setup"}, link: ".fern/setup", want: ".agents/setup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				write(t, dir, f, "#!/bin/sh\n")
			}
			if tt.link != "" {
				outside := filepath.Join(t.TempDir(), "setup")
				write(t, filepath.Dir(outside), "setup", "#!/bin/sh\n")
				if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(tt.link)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(dir, tt.link)); err != nil {
					t.Fatal(err)
				}
			}
			got, err := setup.Script(dir)
			if err != nil || got != tt.want {
				t.Errorf("Script() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// One parser reads services.yaml for the box and for the daemon. The box
// refuses a field it doesn't know; the daemon's readers pass over it, and
// take their own part. Both refuse a field of the wrong type, wherever it
// is.
func TestServicesReaders(t *testing.T) {
	const whole = "services:\n  web:\n    run: vite\n    ready: curl -f localhost:5173\n    port: 5173\n" +
		"golden: {inputs: [seed.sql]}\nlimits: {cpus: 3, memory: 4096}\n"
	parsed := setup.ServicesYAML{
		Services: map[string]setup.Service{"web": {Run: "vite", Ready: "curl -f localhost:5173", Port: 5173}},
		Limits:   setup.Limits{CPUs: 3, Memory: 4096},
	}
	parsed.Golden.Inputs = []string{"seed.sql"}
	for _, tc := range []struct {
		name, yaml string
		box        bool // the box takes it
		daemon     bool // the daemon's readers do
	}{
		{name: "a whole file", yaml: whole, box: true, daemon: true},
		{name: "a field nobody knows", yaml: whole + "restart: always\n", daemon: true},
		{name: "a field of the wrong type", yaml: "services:\n  web:\n    run: vite\n    port: lots\n"},
		{name: "not YAML", yaml: "services: ["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := setup.ParseServices([]byte(tc.yaml))
			if (err == nil) != tc.box {
				t.Errorf("ParseServices() error = %v, want none: %t", err, tc.box)
			}
			if tc.box {
				if diff := cmp.Diff(parsed, got); diff != "" {
					t.Errorf("ParseServices() (-want +got):\n%s", diff)
				}
			}
			dir := t.TempDir()
			write(t, dir, "services.yaml", tc.yaml)
			write(t, dir, "seed.sql", "x")
			limits, err := setup.AskedLimits(dir)
			if (err == nil) != tc.daemon || tc.daemon && limits != parsed.Limits {
				t.Errorf("AskedLimits() = %+v, %v; want %+v, no error: %t", limits, err, parsed.Limits, tc.daemon)
			}
			port, err := setup.PreviewPort(dir)
			if (err == nil) != tc.daemon || tc.daemon && port != 5173 {
				t.Errorf("PreviewPort() = %d, %v; want 5173, no error: %t", port, err, tc.daemon)
			}
			if _, err := setup.Key(dir, "box"); (err == nil) != tc.daemon {
				t.Errorf("Key() error = %v, want none: %t", err, tc.daemon)
			}
		})
	}
}
