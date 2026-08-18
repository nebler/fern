package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nebler/fern/internal/config"
	"gopkg.in/yaml.v3"
)

func runInit(args []string) error {
	flags := newFlagSet("init", "Create a secure single-workspace Fern demo configuration.")
	configPath := flags.String("config", "fern.yaml", "configuration destination")
	envPath := flags.String("env-file", "fern.env", "protected environment destination")
	name := flags.String("name", "demo", "workspace name")
	image := flags.String("image", "fern/opencode:dev", "workspace image")
	repo := flags.String("repo", ".", "repository path")
	memory := flags.String("memory", "8Gi", "workspace memory limit")
	idle := flags.String("idle", "10m", "idle duration")
	listen := flags.String("listen", "127.0.0.1:8080", "proxy listen address")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if *configPath == *envPath {
		return invocationError{message: "configuration and environment destinations must differ"}
	}
	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		return fmt.Errorf("resolve repository: %w", err)
	}
	password := make([]byte, 64)
	if _, err := rand.Read(password); err != nil {
		return fmt.Errorf("generate OpenCode password: %w", err)
	}
	openCodeSecret := hex.EncodeToString(password[:32])
	controlSecret := hex.EncodeToString(password[32:])
	values := config.Default(absRepo)
	values.Workspace.Name = *name
	values.Workspace.Image = *image
	values.Workspace.Memory = *memory
	values.Workspace.Env["OPENCODE_PASSWORD"] = openCodeSecret
	values.Control.Password = controlSecret
	values.Listen = *listen
	parsedIdle, err := time.ParseDuration(*idle)
	if err != nil {
		return err
	}
	values.IdleAfter = parsedIdle
	if err := config.Validate(values); err != nil {
		return err
	}
	type initFile struct {
		Workspace struct {
			Name   string            `yaml:"name"`
			Image  string            `yaml:"image"`
			Repo   string            `yaml:"repo"`
			Memory string            `yaml:"memory"`
			Env    map[string]string `yaml:"env"`
		} `yaml:"workspace"`
		Control struct {
			Password string `yaml:"password"`
		} `yaml:"control"`
		Idle struct {
			After string `yaml:"after"`
		} `yaml:"idle"`
		Proxy struct {
			Listen string `yaml:"listen"`
		} `yaml:"proxy"`
	}
	var output initFile
	output.Workspace.Name = values.Workspace.Name
	output.Workspace.Image = values.Workspace.Image
	output.Workspace.Repo = values.Workspace.Repo
	output.Workspace.Memory = values.Workspace.Memory
	output.Workspace.Env = map[string]string{}
	output.Control.Password = "${FERN_CONTROL_PASSWORD}"
	output.Idle.After = values.IdleAfter.String()
	output.Proxy.Listen = values.Listen
	configData, err := yaml.Marshal(output)
	if err != nil {
		return err
	}
	if err := writeNewFile(*configPath, configData, 0o600); err != nil {
		return err
	}
	envData := []byte("# Keep this file on the Fern host.\nOPENCODE_PASSWORD=" + openCodeSecret + "\nFERN_CONTROL_PASSWORD=" + controlSecret + "\n# Add one provider key, for example:\n# ANTHROPIC_API_KEY=\n")
	if err := writeNewFile(*envPath, envData, 0o600); err != nil {
		_ = os.Remove(*configPath)
		return err
	}
	fmt.Printf("Fern demo configuration created\n\nconfig: %s\nsecrets: %s\nrepository: %s\n\nNext:\n  1. Add a provider key to %s\n  2. Run: fern up --config %s --env-file %s\n  3. In another terminal: tailscale serve --bg http://%s\n  4. In that terminal: fern doctor --config %s --env-file %s --phone\n", *configPath, *envPath, absRepo, *envPath, *configPath, *envPath, *listen, *configPath, *envPath)
	return nil
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent for %q: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing file %q", path)
		}
		return fmt.Errorf("create %q: %w", path, err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("sync %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %q: %w", path, err)
	}
	return nil
}
