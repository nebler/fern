package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/setup"
)

// installTimeout bounds installing a workspace's tools, inside a wake's two
// minutes, and builderInstallTimeout inside a golden builder's eight
// (runsc): what a builder installs, every thread of its repository starts
// with, and a thread's box shouldn't have to.
const (
	installTimeout        = 90 * time.Second
	builderInstallTimeout = 6 * time.Minute
)

// installedFile, in setup.ToolsDir, holds the setup.ToolsKey of what was
// installed there last.
const installedFile = ".installed"

// toolsEnv returns env with the tools mise installs into workspace dir
// (setup.ToolsDir) first on PATH, through mise's shims: they run the version
// the folder a command runs in pins, so a change to mise.toml needs no new
// environment. A workspace that pins none gets env as it is, unless Fern
// guessed its tools (setup.MakeGuess): mise reads those from
// setup.GuessedTools, as its global config, which the repository's own
// files would override.
func toolsEnv(dir string, env []string) []string {
	if key, err := setup.ToolsKey(dir); err != nil || key == "" {
		return env
	}
	var guessed []string
	if g, err := setup.MakeGuess(dir); err == nil && g.Tools != nil {
		guessed = []string{"MISE_GLOBAL_CONFIG_FILE=" + filepath.Join(dir, setup.GuessedTools)}
	}
	data := filepath.Join(dir, setup.ToolsDir)
	out := make([]string, 0, len(env)+5)
	path := "/usr/local/bin:/usr/bin:/bin"
	for _, v := range env {
		if p, ok := strings.CutPrefix(v, "PATH="); ok {
			path = p
			continue
		}
		out = append(out, v)
	}
	return append(append(out,
		"PATH="+filepath.Join(data, "shims")+":"+path,
		"MISE_DATA_DIR="+data,
		// Downloads stay out of the workspace, and so out of its snapshots.
		"MISE_CACHE_DIR=/tmp/mise-cache",
		// The repository's mise.toml is the agent's, like the code it runs.
		"MISE_TRUSTED_CONFIG_PATHS="+dir,
		"MISE_YES=1",
	), guessed...)
}

// installTools installs the tools workspace dir pins into setup.ToolsDir,
// within limit, unless they are installed already: a golden snapshot brings
// them, so a thread's box only checks a key. What a failure means is the
// caller's: a thread's box starts its services anyway, which fail on their
// own if they need a missing tool; a golden builder's doesn't start.
func installTools(ctx context.Context, dir string, env []string, limit time.Duration, log *slog.Logger) error {
	key, err := setup.ToolsKey(dir)
	if err != nil {
		return fmt.Errorf("reading the workspace's tool files: %w", err)
	}
	if key == "" {
		return nil
	}
	data := filepath.Join(dir, setup.ToolsDir)
	if old, err := os.ReadFile(filepath.Join(data, installedFile)); err == nil && string(old) == key {
		return nil
	}
	g, err := setup.MakeGuess(dir)
	if err != nil {
		return fmt.Errorf("guessing the workspace's tools: %w", err)
	}
	start := time.Now()
	if err := install(ctx, dir, data, env, key, g.Tools, limit); err != nil {
		return err
	}
	log.InfoContext(ctx, "installed the workspace's tools", slog.Duration("took", time.Since(start)))
	return nil
}

// install runs mise install in dir, with the guessed tools' config written
// first if there is one.
func install(ctx context.Context, dir, data string, env []string, key string, guessed []byte, limit time.Duration) error {
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	// Ignored by git, itself included, so the tools never show as changes.
	if err := os.WriteFile(filepath.Join(data, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		return err
	}
	if guessed != nil {
		if err := os.WriteFile(filepath.Join(dir, setup.GuessedTools), guessed, 0o644); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mise", "install")
	cmd.Dir, cmd.Env = dir, env
	// A repository's hook can print any amount; the box's memory isn't for that.
	out := boxexec.NewClipped(4000)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("mise install ran past its limit of %s: %s", limit, out.String())
		}
		return fmt.Errorf("mise install: %w: %s", err, out.String())
	}
	return os.WriteFile(filepath.Join(data, installedFile), []byte(key), 0o644)
}
