package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/setup"
)

func TestToolsEnv(t *testing.T) {
	dir := t.TempDir()
	env := []string{"HOME=/tmp", "PATH=/usr/bin:/bin"}
	if diff := cmp.Diff(env, toolsEnv(dir, env)); diff != "" {
		t.Errorf("toolsEnv() of a workspace that pins no tools (-want +got):\n%s", diff)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("node 22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, setup.ToolsDir)
	want := []string{
		"HOME=/tmp",
		"PATH=" + data + "/shims:/usr/bin:/bin",
		"MISE_DATA_DIR=" + data,
		"MISE_CACHE_DIR=/tmp/mise-cache",
		"MISE_TRUSTED_CONFIG_PATHS=" + dir,
		"MISE_YES=1",
	}
	if diff := cmp.Diff(want, toolsEnv(dir, env)); diff != "" {
		t.Errorf("toolsEnv() (-want +got):\n%s", diff)
	}
}

// installTools runs mise install once for each change of the tool files,
// in the workspace, with the tools' environment, and keeps the tools out of
// git.
func TestInstallTools(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	calls := filepath.Join(bin, "calls")
	// A fake mise that records where it ran, with what data folder.
	script := "#!/bin/sh\necho \"$PWD $MISE_DATA_DIR $*\" >>" + calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	pin := func(v string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("[tools]\nnode = \""+v+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.DiscardHandler)
	install := func() []string {
		t.Helper()
		env := toolsEnv(dir, []string{"PATH=" + bin + ":/usr/bin:/bin"})
		installTools(t.Context(), dir, env, log)
		data, err := os.ReadFile(calls)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return slices.DeleteFunc(strings.Split(string(data), "\n"), func(s string) bool { return s == "" })
	}

	if got := install(); len(got) != 0 {
		t.Errorf("mise ran in a workspace that pins no tools: %q", got)
	}
	pin("22")
	data := filepath.Join(dir, setup.ToolsDir)
	want := []string{dir + " " + data + " install"}
	if diff := cmp.Diff(want, install()); diff != "" {
		t.Errorf("mise calls (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want, install()); diff != "" {
		t.Errorf("mise calls with the tools installed (-want +got):\n%s", diff)
	}
	pin("24")
	want = append(want, want[0])
	if diff := cmp.Diff(want, install()); diff != "" {
		t.Errorf("mise calls after a change of mise.toml (-want +got):\n%s", diff)
	}
	if ignore, err := os.ReadFile(filepath.Join(data, ".gitignore")); err != nil || string(ignore) != "*\n" {
		t.Errorf("%s/.gitignore = %q, %v; want \"*\\n\"", setup.ToolsDir, ignore, err)
	}
}

// A failed install doesn't count as installed: the next start tries again.
func TestInstallToolsFails(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte("#!/bin/sh\necho no network >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	if err := os.WriteFile(filepath.Join(dir, ".tool-versions"), []byte("node 22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := toolsEnv(dir, []string{"PATH=" + bin + ":/usr/bin:/bin"})
	installTools(t.Context(), dir, env, slog.New(slog.DiscardHandler))
	if _, err := os.Stat(filepath.Join(dir, setup.ToolsDir, installedFile)); !os.IsNotExist(err) {
		t.Errorf("a failed install left %s: %v", installedFile, err)
	}
}

// A repository with no services.yaml and no pinned tools gets the tools
// Fern guessed, from a config of its own in the tools' folder.
func TestInstallGuessedTools(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$MISE_GLOBAL_CONFIG_FILE $*\" >>" + calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	pkg := `{"scripts": {"dev": "vite"}, "devDependencies": {"vite": "7"}, "packageManager": "pnpm@10.4.1"}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	env := toolsEnv(dir, []string{"PATH=" + bin + ":/usr/bin:/bin"})
	config := filepath.Join(dir, setup.GuessedTools)
	if !slices.Contains(env, "MISE_GLOBAL_CONFIG_FILE="+config) {
		t.Errorf("toolsEnv() = %q, want MISE_GLOBAL_CONFIG_FILE=%s", env, config)
	}
	installTools(t.Context(), dir, env, slog.New(slog.DiscardHandler))
	got, err := os.ReadFile(calls)
	if err != nil || string(got) != config+" install\n" {
		t.Errorf("mise calls = %q, %v; want one install with the guessed config", got, err)
	}
	if data, err := os.ReadFile(config); err != nil || !strings.Contains(string(data), `pnpm = "10.4.1"`) {
		t.Errorf("%s = %q, %v; want the guessed tools", setup.GuessedTools, data, err)
	}
}
