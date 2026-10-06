package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSizeBoundary(t *testing.T) {
	for _, size := range []int{MaxConfigBytes - 1, MaxConfigBytes, MaxConfigBytes + 1} {
		path := filepath.Join(t.TempDir(), "fern.yaml")
		// Pad a valid schema with a comment to isolate the raw byte boundary.
		base := strings.ReplaceAll(currentYAML, "${FERN_CONTROL_PASSWORD}", "test-password") + "#"
		contents := base + strings.Repeat("x", size-len(base))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadWithEnvironment(path, t.TempDir(), true, Overrides{}, nil)
		if size <= MaxConfigBytes && err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if size > MaxConfigBytes && (err == nil || !strings.Contains(err.Error(), "1 MiB limit")) {
			t.Fatalf("oversized config: %v", err)
		}
		if size > MaxConfigBytes {
			if _, loadErr := LoadWithEnvironment(path, t.TempDir(), false, Overrides{}, nil); loadErr == nil || !strings.Contains(loadErr.Error(), "1 MiB limit") {
				t.Fatalf("Load did not enforce size limit: %v", loadErr)
			}
		}
		if err != nil && strings.Contains(err.Error(), contents) {
			t.Fatal("error exposed configuration contents")
		}
	}
}

func TestMissingConfigRequiredPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := LoadWithEnvironment(path, t.TempDir(), false, Overrides{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithEnvironment(path, t.TempDir(), true, Overrides{}, nil); err == nil {
		t.Fatal("required missing config accepted")
	}
}
