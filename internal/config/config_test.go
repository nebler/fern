package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const currentYAML = `workspace:
  name: demo
  repo: .
  github:
    mode: github-app-broker
    hostname: github.com
    installationId: 123
    repository:
      id: 456
      fullName: owner/repository
tasks:
  agent: build
  runtimeStorageRoot: /var/lib/fern-runtime
  model:
    provider: openai
    id: gpt-5
  attemptTimeout: 30m
  leaseDuration: 2m
  backgroundImage: image:test
  backgroundImageID: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  backgroundRoute:
    listen: 127.0.0.1:8443
    origin: https://fern.example.ts.net:8443
control:
  password: ${FERN_CONTROL_PASSWORD}
proxy:
  listen: 127.0.0.1:8080
  operatorListen: 127.0.0.1:8081
  remoteOrigin: https://fern.example.ts.net
`

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadConfig(t *testing.T, data string) (Config, error) {
	t.Helper()
	path := writeConfig(t, data)
	return LoadWithEnvironment(path, filepath.Dir(path), true, Overrides{}, map[string]string{"FERN_CONTROL_PASSWORD": strings.Repeat("s", 32)})
}

func validConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := loadConfig(t, currentYAML)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCurrentConfiguration(t *testing.T) {
	t.Parallel()
	cfg := validConfig(t)
	if cfg.Tasks.RuntimeStorageRoot != "/var/lib/fern-runtime" {
		t.Fatalf("runtime storage root = %q", cfg.Tasks.RuntimeStorageRoot)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.GitHub.InstallationID != 123 || cfg.Workspace.GitHub.Repository.ID != 456 ||
		cfg.Workspace.GitHub.Repository.FullName != "owner/repository" || cfg.Tasks.Agent != "build" ||
		cfg.Tasks.Model.Provider != "openai" || cfg.Tasks.Model.ID != "gpt-5" || cfg.Tasks.AttemptTimeout != 30*time.Minute ||
		cfg.Tasks.LeaseDuration != 2*time.Minute || cfg.Control.Password != strings.Repeat("s", 32) {
		t.Fatalf("decoded configuration = %+v", cfg)
	}
}

func TestStrictParserRejectsRetiredAndUnknownSettings(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]string{
		"workspace_image":        strings.Replace(currentYAML, "  name: demo", "  image: fern/opencode:dev\n  name: demo", 1),
		"workspace_memory":       strings.Replace(currentYAML, "  name: demo", "  memory: 8Gi\n  name: demo", 1),
		"workspace_env":          strings.Replace(currentYAML, "  name: demo", "  env: {}\n  name: demo", 1),
		"idle":                   currentYAML + "idle:\n  after: 10m\n  mode: freeze\n",
		"idle_empty":             currentYAML + "idle: {}\n",
		"workspace_gh":           strings.Replace(currentYAML, "github-app-broker", "workspace-gh", 1),
		"unknown_mode":           strings.Replace(currentYAML, "github-app-broker", "other", 1),
		"background_environment": strings.Replace(currentYAML, "  agent: build", "  backgroundEnvironment: {}\n  agent: build", 1),
		"budget":                 strings.Replace(currentYAML, "  agent: build", "  budget:\n    maxTurns: 100\n  agent: build", 1),
		"maxTurns":               strings.Replace(currentYAML, "  agent: build", "  maxTurns: 100\n  agent: build", 1),
		"verification":           strings.Replace(currentYAML, "  agent: build", "  verification:\n    checkName: tests\n    argv: [/usr/bin/make, test]\n    workingDirectory: ''\n    timeout: 1m\n    outputBytes: 4096\n  agent: build", 1),
		"unknown_root":           currentYAML + "unknown: true\n",
		"unknown_workspace":      strings.Replace(currentYAML, "  name: demo", "  unknown: true\n  name: demo", 1),
		"unknown_task":           strings.Replace(currentYAML, "  agent: build", "  unknown: true\n  agent: build", 1),
		"unknown_model":          strings.Replace(currentYAML, "    id: gpt-5", "    unknown: true\n    id: gpt-5", 1),
		"unknown_route":          strings.Replace(currentYAML, "    listen: 127.0.0.1:8443", "    unknown: true\n    listen: 127.0.0.1:8443", 1),
		"duplicate":              currentYAML + "workspace: {}\n",
		"trailing_document":      currentYAML + "---\n{}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(t, data); err == nil {
				t.Fatal("retired or unknown setting accepted")
			}
		})
	}
}

func TestRequiredFieldsAndStrictScalarTypes(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"    mode: github-app-broker\n", "      id: 456\n", "      fullName: owner/repository\n", "  agent: build\n", "    provider: openai\n", "    id: gpt-5\n", "  attemptTimeout: 30m\n", "  leaseDuration: 2m\n"} {
		t.Run(strings.TrimSpace(field), func(t *testing.T) {
			if _, err := loadConfig(t, strings.Replace(currentYAML, field, "", 1)); err == nil {
				t.Fatal("required field omitted")
			}
		})
	}
	for _, replacement := range []struct{ old, value string }{
		{"id: 456", "id: '456'"}, {"id: 456", "id: 0456"}, {"id: 456", "id: 0"}, {"id: 456", "id: -1"},
		{"id: 456", "id: 9223372036854775808"}, {"installationId: 123", "installationId: '123'"},
		{"installationId: 123", "installationId: 0"}, {"installationId: 123", "installationId: -1"},
		{"installationId: 123", "installationId: 0123"}, {"fullName: owner/repository", "fullName: owner/repository/extra"},
		{"hostname: github.com", "hostname: enterprise.example"}, {"agent: build", "agent: 1"},
		{"provider: openai", "provider: true"}, {"id: gpt-5", "id: []"},
		{"attemptTimeout: 30m", "attemptTimeout: 30"}, {"leaseDuration: 2m", "leaseDuration: tomorrow"},
		{"backgroundImage: image:test", "backgroundImage: ''"}, {"backgroundImageID: sha256:" + strings.Repeat("b", 64), "backgroundImageID: ''"},
	} {
		t.Run(replacement.value, func(t *testing.T) {
			if _, err := loadConfig(t, strings.Replace(currentYAML, replacement.old, replacement.value, 1)); err == nil {
				t.Fatal("invalid scalar accepted")
			}
		})
	}
	for _, data := range []string{"workspace: {}\ntasks: {}", "workspace: {}", strings.Replace(currentYAML, "tasks:\n  agent: build", "tasks: null\nretired:\n  agent: build", 1)} {
		if _, err := loadConfig(t, data); err == nil {
			t.Fatal("missing required section accepted")
		}
	}
}

func TestBootstrapCannotAuthorizeExecution(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(t, strings.Replace(currentYAML, "    installationId: 123\n", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBootstrap(cfg); err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("pending installation authorized execution")
	}
	cfg.Workspace.GitHub.InstallationID = -1
	if err := ValidateBootstrap(cfg); err == nil {
		t.Fatal("negative installation accepted")
	}
}

func TestValidationBoundsAndDependencies(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Config){
		"name":                     func(c *Config) { c.Workspace.Name = "../outside" },
		"repository_id":            func(c *Config) { c.Workspace.GitHub.Repository.ID = 0 },
		"repository_name":          func(c *Config) { c.Workspace.GitHub.Repository.FullName = "invalid" },
		"repository_path":          func(c *Config) { c.Workspace.Repo += "/missing" },
		"password_missing":         func(c *Config) { c.Control.Password = "" },
		"password_short":           func(c *Config) { c.Control.Password = "short" },
		"agent":                    func(c *Config) { c.Tasks.Agent = "" },
		"agent_long":               func(c *Config) { c.Tasks.Agent = strings.Repeat("x", 129) },
		"provider":                 func(c *Config) { c.Tasks.Model.Provider = "\x00" },
		"model":                    func(c *Config) { c.Tasks.Model.ID = strings.Repeat("x", 257) },
		"attempt_low":              func(c *Config) { c.Tasks.AttemptTimeout = time.Minute - time.Nanosecond },
		"attempt_high":             func(c *Config) { c.Tasks.AttemptTimeout = 24*time.Hour + time.Nanosecond },
		"lease_low":                func(c *Config) { c.Tasks.LeaseDuration = time.Minute - time.Nanosecond },
		"lease_high":               func(c *Config) { c.Tasks.LeaseDuration = 5*time.Minute + time.Nanosecond },
		"lease_exceeds_attempt":    func(c *Config) { c.Tasks.AttemptTimeout = time.Minute },
		"image_missing":            func(c *Config) { c.Tasks.BackgroundImage = "" },
		"image_spaces":             func(c *Config) { c.Tasks.BackgroundImage = " image:test" },
		"image_id":                 func(c *Config) { c.Tasks.BackgroundImageID = "sha256:" + strings.Repeat("B", 64) },
		"route_missing":            func(c *Config) { c.Tasks.BackgroundRoute = nil },
		"route_listener":           func(c *Config) { c.Tasks.BackgroundRoute.Listen = "0.0.0.0:8443" },
		"route_collision":          func(c *Config) { c.Tasks.BackgroundRoute.Listen = c.Listen },
		"route_operator_collision": func(c *Config) { c.Tasks.BackgroundRoute.Listen = c.OperatorListen },
		"route_hostname":           func(c *Config) { c.Tasks.BackgroundRoute.Origin = "https://other.example:8443" },
		"route_port":               func(c *Config) { c.Tasks.BackgroundRoute.Origin = "https://fern.example.ts.net" },
		"route_loopback":           func(c *Config) { c.Tasks.BackgroundRoute.Origin = "https://localhost:8443" },
		"remote_missing":           func(c *Config) { c.RemoteOrigin = "" },
		"remote_collision":         func(c *Config) { c.RemoteOrigin = c.Tasks.BackgroundRoute.Origin },
		"listen_public":            func(c *Config) { c.Listen = "0.0.0.0:8080" },
		"listen_dynamic":           func(c *Config) { c.Listen = "127.0.0.1:0" },
		"operator_public":          func(c *Config) { c.OperatorListen = "0.0.0.0:8081" },
		"operator_collision":       func(c *Config) { c.OperatorListen = "[::1]:8080" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			mutate(&cfg)
			if err := Validate(cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	for _, bounds := range []struct{ attempt, lease time.Duration }{{time.Minute, time.Minute}, {24 * time.Hour, 5 * time.Minute}} {
		cfg := validConfig(t)
		cfg.Tasks.AttemptTimeout, cfg.Tasks.LeaseDuration = bounds.attempt, bounds.lease
		if err := Validate(cfg); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEnvironmentExpansionAndOverrides(t *testing.T) {
	t.Setenv("FERN_CONTROL_PASSWORD", "process-secret")
	path := writeConfig(t, currentYAML)
	protected := strings.Repeat("p", 32)
	cfg, err := LoadWithEnvironment(path, t.TempDir(), true, Overrides{}, map[string]string{"FERN_CONTROL_PASSWORD": protected})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Control.Password != protected || cfg.Workspace.Repo != filepath.Dir(path) {
		t.Fatalf("expansion or relative repository = %+v", cfg)
	}
	repo, name, listen := "child", "override", "127.0.0.1:9000"
	base := t.TempDir()
	path = writeConfig(t, strings.Replace(strings.Replace(currentYAML, "  repo: .", "  repo: [invalid]", 1), "  listen: 127.0.0.1:8080", "  listen: []", 1))
	cfg, err = LoadWithEnvironment(path, base, true, Overrides{Repo: &repo, Name: &name, Listen: &listen}, map[string]string{"FERN_CONTROL_PASSWORD": protected})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.Repo != filepath.Join(base, repo) || cfg.Workspace.Name != name || cfg.Listen != listen {
		t.Fatalf("overrides = %+v", cfg)
	}
	cfg, err = loadConfig(t, strings.Replace(currentYAML, "${FERN_CONTROL_PASSWORD}", "$$literal", 1))
	if err != nil || cfg.Control.Password != "$literal" {
		t.Fatalf("dollar escape: %q, %v", cfg.Control.Password, err)
	}
	if _, err := loadConfig(t, strings.Replace(currentYAML, "${FERN_CONTROL_PASSWORD}", "${FERN_TEST_UNSET_4E9511}", 1)); err == nil {
		t.Fatal("unset reference accepted")
	}
	if _, err := loadConfig(t, strings.Replace(currentYAML, "  repo: .", "  repo: ''", 1)); err == nil {
		t.Fatal("empty repository accepted")
	}
}

func TestTaskPolicyDoesNotExpandEnvironment(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfig(t, strings.Replace(currentYAML, "  agent: build", "  agent: ${FERN_CONTROL_PASSWORD}", 1))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tasks.Agent != "${FERN_CONTROL_PASSWORD}" {
		t.Fatal("task policy captured a host secret")
	}
}

func TestParseRemoteOrigin(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "https://fern.example.ts.net", "https://fern.example.ts.net:8443", "https://127.0.0.1", "https://[::1]"} {
		if got, err := ParseRemoteOrigin(value); err != nil || got != value {
			t.Fatalf("%q: %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"http://fern.example", "https://fern.example/", "https://fern.example:443", "https://fern.example:08443", "https://FERN.example", "https://user@fern.example", "https://fern.example?x=1", "https://fern.example#", "https://fern.example/path", "https://fern.example.", "https://fern.example:0", "https://fern.example:65536"} {
		if _, err := ParseRemoteOrigin(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestMissingConfigAndExample(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := LoadWithEnvironment(path, filepath.Dir(path), true, Overrides{}, nil); err == nil {
		t.Fatal("required file absent")
	}
	cfg, err := LoadWithEnvironment(path, filepath.Dir(path), false, Overrides{}, nil)
	if err != nil || cfg.Workspace.Name != "demo" {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	if err := ValidateBootstrap(cfg); err == nil {
		t.Fatal("defaults authorized bootstrap")
	}
	data, err := os.ReadFile("../../fern.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "sha256:REPLACE_WITH_QUALIFIED_LOCAL_IMAGE_ID", "sha256:"+strings.Repeat("b", 64), 1))
	repo := t.TempDir()
	path = writeConfig(t, string(data))
	cfg, err = LoadWithEnvironment(path, repo, true, Overrides{Repo: &repo}, map[string]string{"FERN_CONTROL_PASSWORD": strings.Repeat("s", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
}
