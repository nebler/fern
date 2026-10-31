package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nebler/fern/internal/setup"
)

// installTimeout bounds installing a workspace's tools, inside a wake's
// two minutes.
const installTimeout = 90 * time.Second

// installedFile, in setup.ToolsDir, holds the setup.ToolsKey of what was
// installed there last.
const installedFile = ".installed"

// toolsEnv returns env with the tools mise installs into workspace dir
// (setup.ToolsDir) first on PATH, through mise's shims: they run the version
// the folder a command runs in pins, so a change to mise.toml needs no new
// environment. A workspace that pins none gets env as it is.
func toolsEnv(dir string, env []string) []string {
	if key, err := setup.ToolsKey(dir); err != nil || key == "" {
		return env
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
	return append(out,
		"PATH="+filepath.Join(data, "shims")+":"+path,
		"MISE_DATA_DIR="+data,
		// Downloads stay out of the workspace, and so out of its snapshots.
		"MISE_CACHE_DIR=/tmp/mise-cache",
		// The repository's mise.toml is the agent's, like the code it runs.
		"MISE_TRUSTED_CONFIG_PATHS="+dir,
		"MISE_YES=1",
	)
}

// installTools installs the tools workspace dir pins into setup.ToolsDir,
// unless they are installed already: a golden snapshot brings them, so a
// thread's box only checks a key. A failure is logged; the services start
// anyway, and fail on their own if they need a missing tool.
func installTools(ctx context.Context, dir string, env []string, log *slog.Logger) {
	key, err := setup.ToolsKey(dir)
	if err != nil {
		log.WarnContext(ctx, "reading the workspace's tool files failed", slog.Any("error", err))
		return
	}
	if key == "" {
		return
	}
	data := filepath.Join(dir, setup.ToolsDir)
	if old, err := os.ReadFile(filepath.Join(data, installedFile)); err == nil && string(old) == key {
		return
	}
	start := time.Now()
	if err := install(ctx, dir, data, env, key); err != nil {
		log.WarnContext(ctx, "installing the workspace's tools failed", slog.Any("error", err))
		return
	}
	log.InfoContext(ctx, "installed the workspace's tools", slog.Duration("took", time.Since(start)))
}

func install(ctx context.Context, dir, data string, env []string, key string) error {
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	// Ignored by git, itself included, so the tools never show as changes.
	if err := os.WriteFile(filepath.Join(data, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mise", "install")
	cmd.Dir, cmd.Env = dir, env
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mise install: %w: %s", err, tail(out.String(), 2000))
	}
	return os.WriteFile(filepath.Join(data, installedFile), []byte(key), 0o644)
}

// tail returns s's last n bytes.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
