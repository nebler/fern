package config

import (
	"strings"
	"testing"
)

func TestRuntimeStorageRootValidation(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"/", "relative", "/var/lib/../runtime", "/var//runtime", "/var/runtime/", "/var/runtime\x00"} {
		t.Run(root, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Tasks.RuntimeStorageRoot = root
			if err := ValidateBootstrap(cfg); err == nil || !strings.Contains(err.Error(), "tasks.runtimeStorageRoot") {
				t.Fatalf("invalid root accepted: %v", err)
			}
		})
	}
	cfg := validConfig(t)
	cfg.Tasks.RuntimeStorageRoot = ""
	if err := ValidateBootstrap(cfg); err != nil {
		t.Fatalf("bootstrap requires storage: %v", err)
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "tasks.runtimeStorageRoot") {
		t.Fatalf("execution without storage accepted: %v", err)
	}
	// Shape validation deliberately does not assert filesystem quota support.
	cfg.Tasks.RuntimeStorageRoot = "/synthetic/unprovisioned/runtime"
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStorageRootYAML(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"null", "42", "[]", "{}", "''"} {
		if _, err := loadConfig(t, strings.Replace(currentYAML, "runtimeStorageRoot: /var/lib/fern-runtime", "runtimeStorageRoot: "+value, 1)); err == nil {
			t.Fatalf("accepted runtimeStorageRoot: %s", value)
		}
	}
	cfg, err := loadConfig(t, strings.Replace(currentYAML, "  runtimeStorageRoot: /var/lib/fern-runtime\n", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBootstrap(cfg); err != nil {
		t.Fatal(err)
	}
}
