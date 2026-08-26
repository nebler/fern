package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestInitCreatesProtectedRunnableConfiguration(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "fern.yaml")
	envPath := filepath.Join(directory, "fern.env")
	if err := runInit([]string{"--config", configPath, "--env-file", envPath, "--repo", repository}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{configPath, envPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, got)
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "OPENCODE_PASSWORD") || !strings.Contains(string(data), "${FERN_CONTROL_PASSWORD}") {
		t.Fatal("generated YAML has an invalid credential reference")
	}
	if !strings.Contains(string(data), "listen: 127.0.0.1:8080") || !strings.Contains(string(data), "operatorListen: 127.0.0.1:8081") {
		t.Fatal("generated YAML does not contain distinct remote and operator listeners")
	}
	if strings.Contains(string(data), "remoteOrigin:") {
		t.Fatal("local-only init unexpectedly emitted proxy.remoteOrigin")
	}
	values, err := readEnvFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(values["OPENCODE_PASSWORD"]) != 64 {
		t.Fatalf("password length = %d, want 64", len(values["OPENCODE_PASSWORD"]))
	}
	if len(values["FERN_CONTROL_PASSWORD"]) != 64 || values["FERN_CONTROL_PASSWORD"] == values["OPENCODE_PASSWORD"] {
		t.Fatal("generated control password is missing or not independent")
	}
	loaded, err := config.LoadWithEnvironment(configPath, directory, true, config.Overrides{}, values)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Workspace.Env = mergeWorkspaceEnvironment(loaded.Workspace.Env, values)
	if err := config.Validate(loaded); err != nil {
		t.Fatalf("generated configuration is invalid: %v", err)
	}
	if err := runInit([]string{"--config", configPath, "--env-file", envPath, "--repo", repository}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second init error = %v", err)
	}
}

func TestInitConditionallyEmitsRemoteOriginAndGuidance(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repo")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "fern.yaml")
	envPath := filepath.Join(directory, "fern.env")
	origin := "https://fern.example.ts.net"
	if err := runInit([]string{"--config", configPath, "--env-file", envPath, "--repo", repository, "--remote-origin", origin}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "remoteOrigin: "+origin) {
		t.Fatalf("generated YAML = %s", data)
	}
	remoteSteps := initNextSteps("fern.yaml", "fern.env", "127.0.0.1:8080", origin)
	if !strings.Contains(remoteSteps, origin) || !strings.Contains(remoteSteps, "doctor") {
		t.Fatalf("remote guidance = %q", remoteSteps)
	}
	localSteps := initNextSteps("fern.yaml", "fern.env", "127.0.0.1:8080", "")
	if !strings.Contains(localSteps, "set proxy.remoteOrigin") || strings.Contains(localSteps, "doctor") {
		t.Fatalf("local-only guidance = %q", localSteps)
	}
}

func TestReadEnvFileRejectsMalformedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fern.env")
	if err := os.WriteFile(path, []byte("GOOD=value\nbad line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(path); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("malformed environment error = %v", err)
	}
}

func TestReadEnvFileRejectsExposedSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fern.env")
	if err := os.WriteFile(path, []byte("OPENCODE_PASSWORD=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("exposed environment error = %v", err)
	}
}

func TestWorkspaceEnvironmentForwardsOnlySupportedImplicitKeys(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"OPENCODE_PASSWORD": "open-secret", "FERN_CONTROL_PASSWORD": "control-secret",
		"ANTHROPIC_API_KEY": "provider-secret", "AWS_SECRET_ACCESS_KEY": "host-secret",
	}
	merged := mergeWorkspaceEnvironment(map[string]string{"EXPLICIT": "configured"}, values)
	if merged["OPENCODE_PASSWORD"] != "open-secret" || merged["ANTHROPIC_API_KEY"] != "provider-secret" || merged["EXPLICIT"] != "configured" {
		t.Fatalf("supported workspace environment missing: %+v", merged)
	}
	for _, key := range []string{"FERN_CONTROL_PASSWORD", "AWS_SECRET_ACCESS_KEY"} {
		if _, exists := merged[key]; exists {
			t.Fatalf("host-only %s entered workspace environment", key)
		}
	}
}

func TestWriteQRDoesNotExposeCredentials(t *testing.T) {
	var output strings.Builder
	value := "https://fern.example.ts.net/fern/"
	if err := writeQR(&output, value); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 || strings.Contains(output.String(), value) {
		t.Fatal("QR output is empty or contains the literal URL")
	}
}

// tailscaleOrigin extracts the single HTTPS origin advertised anywhere in a
// `tailscale serve status` output. Only tests consume it directly; production
// code uses the target- and topology-scoped variants below.
func tailscaleOrigin(output string) (string, error) {
	matches := httpsOriginPattern.FindAllString(output, -1)
	unique := make(map[string]bool)
	for _, match := range matches {
		unique[match] = true
	}
	if len(unique) != 1 {
		return "", fmt.Errorf("expected one Tailscale HTTPS origin, found %d", len(unique))
	}
	for origin := range unique {
		return origin, nil
	}
	return "", errors.New("no Tailscale HTTPS origin")
}

func TestTailscaleOrigin(t *testing.T) {
	t.Parallel()
	output := "Available within your tailnet:\n\nhttps://fern-host.example.ts.net\n|-- / proxy http://127.0.0.1:8080\n"
	if got, err := tailscaleOrigin(output); err != nil || got != "https://fern-host.example.ts.net" {
		t.Fatalf("tailscale origin = %q, %v", got, err)
	}
	if _, err := tailscaleOrigin("https://one.ts.net https://two.ts.net"); err == nil {
		t.Fatal("accepted ambiguous Tailscale origins")
	}
	if got, err := tailscaleOriginForTarget(output, "127.0.0.1:8080"); err != nil || got != "https://fern-host.example.ts.net" {
		t.Fatalf("targeted Tailscale origin = %q, %v", got, err)
	}
	if _, err := tailscaleOriginForTarget(output, "127.0.0.1:9090"); err == nil {
		t.Fatal("accepted wrong Tailscale Serve target")
	}
	if _, err := tailscaleOriginForTarget(output+"Funnel on\n", "127.0.0.1:8080"); err == nil {
		t.Fatal("accepted Tailscale Funnel")
	}
	multiple := "https://other.example.ts.net\n|-- / proxy http://127.0.0.1:9000\n\nhttps://fern-host.example.ts.net:8443\n|-- / proxy http://127.0.0.1:8080\n"
	if got, err := tailscaleOriginForTarget(multiple, "127.0.0.1:8080"); err != nil || got != "https://fern-host.example.ts.net:8443" {
		t.Fatalf("block-associated Tailscale origin = %q, %v", got, err)
	}
}

func TestTailscaleLocalOrigin(t *testing.T) {
	t.Parallel()
	if got, err := tailscaleLocalOrigin([]byte(`{"BackendState":"Running","Self":{"DNSName":"fern-host.example.ts.net."}}`)); err != nil || got != "https://fern-host.example.ts.net" {
		t.Fatalf("local origin = %q, %v", got, err)
	}
	if _, err := tailscaleLocalOrigin([]byte(`{"BackendState":"NeedsLogin","Self":{"DNSName":"fern-host.example.ts.net."}}`)); err == nil {
		t.Fatal("accepted inactive Tailscale backend")
	}
}
