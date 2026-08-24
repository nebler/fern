package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeIdleConfig(t *testing.T, idleBlock string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	// A control password and workspace OPENCODE_PASSWORD are included so the
	// full Validate gauntlet reaches the idle.mode checks under test.
	content := "workspace:\n  repo: .\n  env:\n    OPENCODE_PASSWORD: opencode-secret-opencode-secret-32\ncontrol:\n  password: fern-control-secret-fern-control-32\n" + idleBlock
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIdleModeDefaultsToStop(t *testing.T) {
	t.Parallel()
	path := writeIdleConfig(t, "")
	loaded, err := Load(path, t.TempDir(), true, Overrides{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.IdleMode != IdleModeStop {
		t.Fatalf("default idle.mode = %q, want %q", loaded.IdleMode, IdleModeStop)
	}
}

func TestIdleModeParsesFreeze(t *testing.T) {
	t.Parallel()
	path := writeIdleConfig(t, "idle:\n  after: 5m\n  mode: freeze\n")
	loaded, err := Load(path, t.TempDir(), true, Overrides{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.IdleMode != IdleModeFreeze || loaded.IdleAfter.String() != "5m0s" {
		t.Fatalf("idle = mode %q after %v", loaded.IdleMode, loaded.IdleAfter)
	}
}

func TestIdleModeRejectsUnknownMechanism(t *testing.T) {
	t.Parallel()
	path := writeIdleConfig(t, "idle:\n  mode: sigkill\n")
	loaded, err := Load(path, t.TempDir(), true, Overrides{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = Validate(loaded)
	if err == nil || !strings.Contains(err.Error(), `idle.mode must be "stop" or "freeze"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestIdleModeRejectsUnknownSiblingField(t *testing.T) {
	t.Parallel()
	path := writeIdleConfig(t, "idle:\n  after: 10m\n  modde: freeze\n")
	if _, err := Load(path, t.TempDir(), true, Overrides{}); err == nil {
		t.Fatal("strict decoding accepted an unknown field under idle")
	}
}

func TestIdleModeOverrideWinsOverFile(t *testing.T) {
	t.Parallel()
	path := writeIdleConfig(t, "idle:\n  mode: freeze\n")
	mode := IdleModeStop
	loaded, err := Load(path, t.TempDir(), true, Overrides{IdleMode: &mode})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.IdleMode != IdleModeStop {
		t.Fatalf("override ignored: idle.mode = %q", loaded.IdleMode)
	}
}

func TestIdleModeFlagValidationCatchesBadValueBeforeLoadDefaults(t *testing.T) {
	t.Parallel()
	path := writeIdleConfig(t, "")
	bad := "thaw-only"
	loaded, err := Load(path, t.TempDir(), true, Overrides{IdleMode: &bad})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = Validate(loaded)
	if err == nil || !strings.Contains(err.Error(), `idle.mode must be "stop" or "freeze"`) {
		t.Fatalf("err = %v", err)
	}
}
