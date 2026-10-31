package setup_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"

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

func TestID(t *testing.T) {
	dns := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	ids := map[string]bool{}
	for _, repo := range []string{"sample", "My_App", "my-app", "___", "a-very-long-repository-name-that-goes-on-and-on"} {
		id := setup.ID(repo)
		if !dns.MatchString(id) {
			t.Errorf("ID(%q) = %q, not a DNS label", repo, id)
		}
		if ids[id] {
			t.Errorf("ID(%q) = %q, the same as another repository's", repo, id)
		}
		ids[id] = true
	}
}

func TestSourceID(t *testing.T) {
	dns := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	for _, repo := range []string{"sample", "src", "a-very-long-repository-name-that-goes-on-and-on"} {
		id := setup.SourceID(repo)
		if !dns.MatchString(id) {
			t.Errorf("SourceID(%q) = %q, not a DNS label", repo, id)
		}
		if got, ok := setup.BuilderOf(id); !ok || got != setup.ID(repo) {
			t.Errorf("BuilderOf(SourceID(%q)) = %q, %v; want %q, true", repo, got, ok, setup.ID(repo))
		}
		if _, ok := setup.BuilderOf(setup.ID(repo)); ok {
			t.Errorf("BuilderOf(ID(%q)) is a source, want not", repo)
		}
	}
	for _, id := range []string{"t3-abc-src", "src"} {
		if _, ok := setup.BuilderOf(id); ok {
			t.Errorf("BuilderOf(%q) is a source, want not: no builder's", id)
		}
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
