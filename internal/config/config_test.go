package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseMemoryBytes(t *testing.T) {
	t.Parallel()
	tests := map[string]int64{
		"8Gi":   8 * 1024 * 1024 * 1024,
		"512Mi": 512 * 1024 * 1024,
		"2GB":   2_000_000_000,
		"1024":  1024 * 1024 * 1024,
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseMemoryBytes(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("ParseMemoryBytes(%q) = %d, want %d", input, got, want)
			}
		})
	}
}

func TestValidateRequiresLoopbackListen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		address string
		wantErr bool
	}{
		{address: "127.0.0.1:8080"},
		{address: "[::1]:8080"},
		{address: "0.0.0.0:8080", wantErr: true},
		{address: "[::]:8080", wantErr: true},
		{address: "192.168.1.2:8080", wantErr: true},
		{address: "100.64.0.1:8080", wantErr: true},
		{address: "localhost:8080", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			t.Parallel()
			config := Default(t.TempDir())
			config.Listen = test.address
			config.Workspace.Env["OPENCODE_PASSWORD"] = "secret"
			config.Control.Password = "control-secret-control-secret-1234"
			err := Validate(config)
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestValidateRequiresDistinctLoopbackOperatorListen(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"localhost:8081", "0.0.0.0:8081", "100.64.0.1:8081", "127.0.0.1:0", "127.0.0.1:8080", "127.0.0.2:8080", "[::ffff:127.0.0.1]:8080"} {
		address := address
		t.Run(address, func(t *testing.T) {
			t.Parallel()
			value := Default(t.TempDir())
			value.OperatorListen = address
			value.Workspace.Env["OPENCODE_PASSWORD"] = "secret"
			value.Control.Password = "control-secret-control-secret-1234"
			if err := Validate(value); err == nil {
				t.Fatalf("Validate accepted operator listener %q", address)
			}
		})
	}
}

func TestValidateRequiresAuthentication(t *testing.T) {
	t.Parallel()
	config := Default(t.TempDir())
	if err := Validate(config); err == nil {
		t.Fatal("Validate accepted missing OPENCODE_PASSWORD")
	}
	config.Workspace.Env["OPENCODE_PASSWORD"] = "secret"
	config.Control.Password = "control-secret-control-secret-1234"
	if err := Validate(config); err != nil {
		t.Fatal(err)
	}
	config.Control.Password = "short-control-secret"
	if err := Validate(config); err == nil {
		t.Fatal("Validate accepted a short control password")
	}
	config.Control.Password = "secret"
	if err := Validate(config); err == nil {
		t.Fatal("Validate accepted identical OpenCode and control passwords")
	}
}

func TestParseRemoteOrigin(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "https://fern.example.ts.net", "https://fern.example.ts.net:8443", "https://127.0.0.1:444", "https://[2001:db8::1]"} {
		value := value
		t.Run("valid_"+strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			t.Parallel()
			if got, err := ParseRemoteOrigin(value); err != nil || got != value {
				t.Fatalf("ParseRemoteOrigin(%q) = %q, %v", value, got, err)
			}
		})
	}
	invalid := []string{
		"http://fern.example", "fern.example", "//fern.example", "https:fern.example", "https://",
		"https://user@fern.example", "https://fern.example/", "https://fern.example/path",
		"https://fern.example?", "https://fern.example?a=b", "https://fern.example#", "https://fern.example#part",
		"https://fern.example.", "https://Fern.example", "HTTPS://fern.example", "https://fern_example",
		"https://-fern.example", "https://fern-.example", "https://fern..example",
		"https://fern.example:0", "https://fern.example:443", "https://fern.example:0443", "https://fern.example:65536", "https://fern.example:invalid",
		"https://[2001:0DB8::1]", "https://[2001:db8::1]:443", "mailto:fern@example.com",
	}
	for _, value := range invalid {
		value := value
		t.Run("invalid_"+strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			t.Parallel()
			if _, err := ParseRemoteOrigin(value); err == nil {
				t.Fatalf("ParseRemoteOrigin accepted %q", value)
			}
		})
	}
}

func TestLoadRemoteOriginIsOptionalAndStrict(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: .\nproxy:\n  remoteOrigin: https://fern.example.ts.net\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil || loaded.RemoteOrigin != "https://fern.example.ts.net" {
		t.Fatalf("loaded remote origin = %q, %v", loaded.RemoteOrigin, err)
	}
	if err := os.WriteFile(path, []byte("workspace:\n  repo: .\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(path, directory, true, Overrides{})
	if err != nil || loaded.RemoteOrigin != "" {
		t.Fatalf("absent remote origin = %q, %v", loaded.RemoteOrigin, err)
	}
}

func TestLoadGitHubRepositoryBindingIsOptionalStrictAndRetained(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	valid := "workspace:\n  repo: .\n  github:\n    mode: workspace-gh\n    hostname: github.com\n    repository:\n      id: 987654321\n      fullName: Owner-Name/repo.name\n"
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, load := range []func(string, string, bool, Overrides) (Config, error){Load, LoadWorkspace} {
		loaded, err := load(path, directory, true, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Workspace.GitHub == nil || loaded.Workspace.GitHub.Repository.ID != 987654321 || loaded.Workspace.GitHub.Repository.FullName != "Owner-Name/repo.name" {
			t.Fatalf("binding = %+v", loaded.Workspace.GitHub)
		}
	}
	if err := os.WriteFile(path, []byte("workspace:\n  repo: .\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil || loaded.Workspace.GitHub != nil {
		t.Fatalf("missing binding = %+v, %v", loaded.Workspace.GitHub, err)
	}
}

func TestLoadRejectsNoncanonicalGitHubRepositoryBinding(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	invalidRepositories := []string{"-owner/repo", "owner-/repo", "owner/repo.git", "owner/repo/Git", "owner/repo name", "owner/.."}
	for _, fullName := range invalidRepositories {
		data := "workspace:\n  repo: .\n  github:\n    repository:\n      id: 1\n      fullName: " + fullName + "\n"
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path, directory, true, Overrides{}); err == nil {
			t.Errorf("Load accepted repository %q", fullName)
		}
	}
	for _, id := range []string{"", "0", "-1", "+1", "01", "1.0", "'1'", "9223372036854775808"} {
		data := "workspace:\n  repo: .\n  github:\n    repository:\n      id: " + id + "\n      fullName: owner/repo\n"
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path, directory, true, Overrides{}); err == nil {
			t.Errorf("Load accepted repository ID %q", id)
		}
	}
	for _, data := range []string{
		"workspace:\n  repo: .\n  github: {}\n",
		"workspace:\n  repo: .\n  github:\n    repository:\n      id: 1\n",
		"workspace:\n  repo: .\n  github:\n    repository:\n      id: 1\n      fullName: owner/repo\n      unknown: true\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path, directory, true, Overrides{}); err == nil {
			t.Errorf("Load accepted invalid nested binding:\n%s", data)
		}
	}
}

func TestLoadWithEnvironmentExpandsProtectedValues(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  repo: ${FERN_TEST_REPO}\n  env:\n    OPENCODE_PASSWORD: ${OPENCODE_PASSWORD}\ncontrol:\n  password: ${FERN_CONTROL_PASSWORD}\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadWithEnvironment(path, directory, true, Overrides{}, map[string]string{
		"FERN_TEST_REPO": repository, "OPENCODE_PASSWORD": "protected-secret", "FERN_CONTROL_PASSWORD": "control-secret-control-secret-1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Workspace.Repo != repository || config.Workspace.Env["OPENCODE_PASSWORD"] != "protected-secret" || config.Control.Password != "control-secret-control-secret-1234" {
		t.Fatalf("loaded config = %+v", config)
	}
}

func TestValidateRejectsHostGitHubCredentials(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"FERN_CONTROL_PASSWORD", "FERN_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			value := Default(t.TempDir())
			value.Workspace.Env["OPENCODE_PASSWORD"] = "secret"
			value.Control.Password = "control-secret-control-secret-1234"
			value.Workspace.Env[key] = "must-stay-on-host"
			if err := Validate(value); err == nil {
				t.Fatalf("Validate accepted host-only %s", key)
			}
		})
	}
}

func TestLoadPasswordForWorkspaceAndClients(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  repo: .\n  env:\n    OPENCODE_PASSWORD: secret\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	attach, err := LoadAttach(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := LoadEvents(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Env["OPENCODE_PASSWORD"] != "secret" || attach.Env["OPENCODE_PASSWORD"] != "secret" || events.Env["OPENCODE_PASSWORD"] != "secret" {
		t.Fatal("client projections did not load OPENCODE_PASSWORD")
	}
}

func TestClientProjectionsExpandFromCallerEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    OPENCODE_PASSWORD: ${OPENCODE_PASSWORD}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(key string) (string, bool) {
		if key == "OPENCODE_PASSWORD" {
			return "env-file-secret", true
		}
		return "", false
	}
	attach, err := LoadAttachWithEnvironment(path, true, nil, lookup)
	if err != nil {
		t.Fatal(err)
	}
	events, err := LoadEventsWithEnvironment(path, true, nil, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if attach.Env["OPENCODE_PASSWORD"] != "env-file-secret" || events.Env["OPENCODE_PASSWORD"] != "env-file-secret" {
		t.Fatal("client projections did not use caller environment")
	}
}

func TestLoadRejectsHostOnlySecretAliasedIntoWorkspace(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for _, reference := range []string{"${FERN_CONTROL_PASSWORD}", "$FERN_GITHUB_TOKEN", "prefix-${GH_TOKEN}", "${GITHUB_TOKEN}-suffix"} {
		path := filepath.Join(directory, strings.NewReplacer("$", "d", "{", "", "}", "").Replace(reference)+".yaml")
		data := []byte("workspace:\n  repo: .\n  env:\n    OTHER_TOKEN: " + reference + "\n")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadWithEnvironment(path, directory, true, Overrides{}, map[string]string{
			"FERN_CONTROL_PASSWORD": "host-secret", "FERN_GITHUB_TOKEN": "host-secret",
			"GH_TOKEN": "host-secret", "GITHUB_TOKEN": "host-secret",
		}); err == nil || !strings.Contains(err.Error(), "host-only") {
			t.Errorf("reference %q error = %v", reference, err)
		}
	}
}

func TestLoadAllowsEscapedHostOnlyVariableName(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  repo: .\n  env:\n    LITERAL: $${FERN_CONTROL_PASSWORD}\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithEnvironment(path, directory, true, Overrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Env["LITERAL"] != "${FERN_CONTROL_PASSWORD}" {
		t.Fatalf("escaped value = %q", loaded.Workspace.Env["LITERAL"])
	}
}

func TestLoadRejectsHostOnlySecretValueThroughSecondAlias(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  repo: .\n  env:\n    OTHER_TOKEN: prefix-${ALIAS}-suffix\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "github-secret-value-that-must-stay-host-only"
	_, err := LoadWithEnvironment(path, directory, true, Overrides{}, map[string]string{
		"ALIAS": secret, "GH_TOKEN": secret,
	})
	if err == nil || !strings.Contains(err.Error(), "host-only GH_TOKEN") {
		t.Fatalf("LoadWithEnvironment() error = %v", err)
	}
}

func TestValidateRejectsControlCredentialUnderWorkspaceAlias(t *testing.T) {
	t.Parallel()
	config := Default(t.TempDir())
	config.Workspace.Env["OPENCODE_PASSWORD"] = "opencode-secret"
	config.Control.Password = strings.Repeat("c", 32)
	config.Workspace.Env["OTHER_TOKEN"] = config.Control.Password
	if err := Validate(config); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestLoadRejectsRemovedWorkspaceKey(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  opencode: v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
		t.Fatal("Load accepted removed workspace.opencode")
	}
}

func TestDefaultUsesV2Image(t *testing.T) {
	t.Parallel()
	if got := Default(t.TempDir()).Workspace.Image; got != "fern/opencode:dev" {
		t.Fatalf("default image = %q", got)
	}
}

func TestValidateRejectsDynamicProxyPort(t *testing.T) {
	t.Parallel()
	config := Default(t.TempDir())
	config.Listen = "127.0.0.1:0"
	if err := Validate(config); err == nil {
		t.Fatal("Validate accepted proxy port 0")
	}
}

func TestLoadAppliesOverridesBeforeNormalization(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	repo := filepath.Join(directory, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: ${MISSING_REPO}\nidle:\n  after: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idle := "1m"
	loaded, err := Load(path, directory, true, Overrides{Repo: &repo, IdleAfter: &idle})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Repo != repo || loaded.IdleAfter != time.Minute {
		t.Fatalf("loaded repo=%q idle=%s", loaded.Workspace.Repo, loaded.IdleAfter)
	}
}

func TestLoadOverridesInvalidYAMLTypes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	repo := filepath.Join(directory, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  repo: [invalid]\nidle:\n  after: [invalid]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	idle := "1m"
	if _, err := Load(path, directory, true, Overrides{Repo: &repo, IdleAfter: &idle}); err != nil {
		t.Fatalf("valid overrides did not replace invalid YAML types: %v", err)
	}
}

func TestLoadRejectsEmptyRepository(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
		t.Fatal("Load accepted an empty repository")
	}
	empty := ""
	if _, err := Load(path, directory, true, Overrides{Repo: &empty}); err == nil {
		t.Fatal("Load accepted an explicitly empty repository override")
	}
}

func TestLoadWorkspaceIgnoresInvalidIdleConfiguration(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: .\nidle:\n  after: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(path, directory, true, Overrides{}); err != nil {
		t.Fatalf("workspace-only load rejected idle configuration: %v", err)
	}
	if err := os.WriteFile(path, []byte("workspace:\n  naem: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(path, directory, true, Overrides{}); err == nil {
		t.Fatal("workspace-only load accepted an unknown workspace field")
	}
}

func TestLoadWorkspaceRejectsDuplicateWorkspaceSections(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := []byte("workspace:\n  name: first\nworkspace:\n  name: second\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspace(path, directory, true, Overrides{}); err == nil {
		t.Fatal("LoadWorkspace accepted duplicate workspace sections")
	}
}

func TestLoadAttachPreservesExplicitEmptyPassword(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    OPENCODE_PASSWORD: ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := LoadAttach(path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	password, exists := client.Env["OPENCODE_PASSWORD"]
	if !exists || password != "" {
		t.Fatalf("explicit password was not preserved: value=%q exists=%t", password, exists)
	}
}

func TestClientProjectionsIgnoreUnrelatedMalformedValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	data := []byte("workspace:\n  name: demo\n  env:\n    UNUSED:\n      nested: value\nproxy:\n  listen: [invalid]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadEvents(path, true, nil); err != nil {
		t.Fatalf("event projection parsed unrelated values: %v", err)
	}
	data = []byte("workspace:\n  name: [invalid]\n  env:\n    UNUSED:\n      nested: value\nproxy:\n  operatorListen: 127.0.0.1:9090\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := LoadAttach(path, true, nil)
	if err != nil {
		t.Fatalf("attach projection parsed unrelated values: %v", err)
	}
	if client.Listen != "127.0.0.1:9090" {
		t.Fatalf("attach listen = %q", client.Listen)
	}
}

func TestClientOverridesSkipInvalidRelevantYAML(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	data := []byte("workspace:\n  name: [invalid]\nproxy:\n  listen: [invalid]\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	listen := "127.0.0.1:9090"
	if _, err := LoadAttach(path, true, &listen); err != nil {
		t.Fatalf("attach override did not skip invalid YAML: %v", err)
	}
	name := "demo"
	if _, err := LoadEvents(path, true, &name); err != nil {
		t.Fatalf("event override did not skip invalid YAML: %v", err)
	}
}

func TestLoadWorkspaceNameRejectsTrailingDocument(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fern.yaml")
	data := []byte("workspace:\n  name: production\n---\nworkspace:\n  name: staging\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkspaceName(path, true); err == nil {
		t.Fatal("LoadWorkspaceName accepted multiple YAML documents")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  naem: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
		t.Fatal("Load accepted an unknown field")
	}
}

func TestLoadRejectsMissingEnvironmentReference(t *testing.T) {
	previous, existed := os.LookupEnv("FERN_TEST_MISSING")
	_ = os.Unsetenv("FERN_TEST_MISSING")
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("FERN_TEST_MISSING", previous)
		}
	})
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    TOKEN: ${FERN_TEST_MISSING}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
		t.Fatal("Load accepted a missing environment variable")
	}
}

func TestLoadSupportsEscapedDollarAndRejectsTrailingDocument(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  env:\n    PRICE: '$$5'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace.Env["PRICE"] != "$5" {
		t.Fatalf("escaped value = %q", loaded.Workspace.Env["PRICE"])
	}
	if err := os.WriteFile(path, []byte("workspace: {}\n---\nworkspace: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, directory, true, Overrides{}); err == nil {
		t.Fatal("Load accepted multiple YAML documents")
	}
}

func TestLoadResolvesRepoRelativeToConfig(t *testing.T) {
	directory := t.TempDir()
	repo := filepath.Join(directory, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.yaml")
	if err := os.WriteFile(path, []byte("workspace:\n  repo: ./repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, "/wrong", true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Workspace.Repo != repo {
		t.Fatalf("repo = %q, want %q", config.Workspace.Repo, repo)
	}
}

func TestLoadCompleteTaskPolicy(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := `workspace:
  repo: .
  env:
    OPENCODE_PASSWORD: opencode-secret
  github:
    mode: github-app-broker
    hostname: github.com
    installationId: 123456
    repository:
      id: 987654321
      fullName: owner/repository
control:
  password: control-secret-control-secret-1234
proxy:
  remoteOrigin: https://fern.example.ts.net
tasks:
  agent: build
  model:
    provider: openai
    id: gpt-5
  attemptTimeout: 30m
  leaseDuration: 2m
  backgroundImage: fern/opencode-background-source:dev
  backgroundImageID: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  backgroundRoute:
    listen: 127.0.0.1:8443
    origin: https://fern.example.ts.net:8443
  budget:
    maxTurns: 100
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tasks == nil {
		t.Fatal("tasks were not loaded")
	}
	if loaded.Workspace.GitHub == nil || loaded.Workspace.GitHub.InstallationID != 123456 {
		t.Fatalf("GitHub binding = %+v", loaded.Workspace.GitHub)
	}
	want := TaskPolicy{
		Agent: "build", Model: TaskModel{Provider: "openai", ID: "gpt-5"},
		AttemptTimeout: 30 * time.Minute, LeaseDuration: 2 * time.Minute,
		Budget: TaskBudget{MaxTurns: 100}, BackgroundImage: "fern/opencode-background-source:dev", BackgroundEnvironment: map[string]string{},
		BackgroundImageID: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		BackgroundRoute:   &BackgroundRoute{Listen: "127.0.0.1:8443", Origin: "https://fern.example.ts.net:8443"},
	}
	if !reflect.DeepEqual(*loaded.Tasks, want) {
		t.Fatalf("tasks = %+v, want %+v", *loaded.Tasks, want)
	}
	if err := Validate(loaded); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTaskPolicyRequiresEveryField(t *testing.T) {
	t.Parallel()
	complete := map[string]string{
		"agent":          "  agent: build\n",
		"model":          "  model:\n    provider: openai\n    id: gpt-5\n",
		"attemptTimeout": "  attemptTimeout: 30m\n",
		"leaseDuration":  "  leaseDuration: 2m\n",
		"budget":         "  budget:\n    maxTurns: 100\n",
	}
	for missing := range complete {
		missing := missing
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			var data strings.Builder
			data.WriteString("workspace:\n  repo: .\ntasks:\n")
			for _, name := range []string{"agent", "model", "attemptTimeout", "leaseDuration", "budget"} {
				if name != missing {
					data.WriteString(complete[name])
				}
			}
			path := filepath.Join(t.TempDir(), "fern.yaml")
			if err := os.WriteFile(path, []byte(data.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path, filepath.Dir(path), true, Overrides{}); err == nil {
				t.Fatalf("Load accepted tasks without %s", missing)
			}
		})
	}
	for _, test := range []struct {
		name string
		data string
	}{
		{"model_provider", "  model:\n    id: gpt-5\n"},
		{"model_id", "  model:\n    provider: openai\n"},
		{"budget_maxTurns", "  budget: {}\n"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			data := "workspace:\n  repo: .\ntasks:\n  agent: build\n  attemptTimeout: 30m\n  leaseDuration: 2m\n" + test.data
			if test.name != "budget_maxTurns" {
				data += "  budget:\n    maxTurns: 100\n"
			}
			path := filepath.Join(t.TempDir(), "fern.yaml")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path, filepath.Dir(path), true, Overrides{}); err == nil {
				t.Fatalf("Load accepted tasks without %s", test.name)
			}
		})
	}
}

func TestLoadOptionalVerificationPolicyIsStrictAndCopied(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := `workspace:
  repo: .
tasks:
  agent: build
  model:
    provider: openai
    id: gpt-5
  attemptTimeout: 30m
  leaseDuration: 2m
  budget:
    maxTurns: 100
  verification:
    checkName: repository-tests
    argv: [/usr/bin/make, test]
    workingDirectory: ""
    timeout: 15m
    environment:
      CI: "true"
    outputBytes: 65536
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	verification := loaded.Tasks.Verification
	if verification == nil || verification.CheckName != "repository-tests" || verification.Timeout != 15*time.Minute ||
		verification.WorkingDirectory != "" || verification.OutputBytes != 65536 || len(verification.Argv) != 2 ||
		verification.Argv[0] != "/usr/bin/make" || verification.Argv[1] != "test" || verification.Environment["CI"] != "true" {
		t.Fatalf("verification = %+v", verification)
	}
	if err := ValidateWorkspace(loaded); err != nil {
		t.Fatal(err)
	}
	verification.Argv[0] = "changed"
	verification.Environment["CI"] = "changed"
	if strings.Contains(data, "changed") {
		t.Fatal("test fixture unexpectedly aliased")
	}
}

func TestValidateVerificationPolicyBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*TaskVerificationPolicy)
	}{
		{"name", func(value *TaskVerificationPolicy) { value.CheckName = "Bad Name" }},
		{"argv_empty", func(value *TaskVerificationPolicy) { value.Argv = nil }},
		{"argv_relative", func(value *TaskVerificationPolicy) { value.Argv[0] = "make" }},
		{"argv_nul", func(value *TaskVerificationPolicy) { value.Argv = append(value.Argv, "bad\x00arg") }},
		{"working_directory", func(value *TaskVerificationPolicy) { value.WorkingDirectory = "../outside" }},
		{"timeout", func(value *TaskVerificationPolicy) { value.Timeout = time.Hour + time.Nanosecond }},
		{"output", func(value *TaskVerificationPolicy) { value.OutputBytes = 1<<20 + 1 }},
		{"environment_name", func(value *TaskVerificationPolicy) { value.Environment["BAD-NAME"] = "x" }},
		{"environment_nul", func(value *TaskVerificationPolicy) { value.Environment["VALID"] = "bad\x00value" }},
		{"environment_reserved", func(value *TaskVerificationPolicy) { value.Environment["PATH"] = "/other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := validTaskConfig(t)
			config.Tasks.Verification = &TaskVerificationPolicy{
				CheckName: "tests", Argv: []string{"/usr/bin/make", "test"}, Timeout: time.Minute,
				Environment: map[string]string{"CI": "true"}, OutputBytes: 4096,
			}
			test.mutate(config.Tasks.Verification)
			if err := Validate(config); err == nil {
				t.Fatal("Validate accepted invalid verification policy")
			}
		})
	}
}

func TestValidateVerificationExecutableOutsideWorkspace(t *testing.T) {
	t.Parallel()
	config := validTaskConfig(t)
	executable := filepath.Join(config.Workspace.Repo, "verification-check")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config.Tasks.Verification = &TaskVerificationPolicy{
		CheckName: "tests", Argv: []string{executable}, Timeout: time.Minute,
		Environment: map[string]string{}, OutputBytes: 4096,
	}
	if err := Validate(config); err == nil {
		t.Fatal("Validate accepted a check executable from the writable workspace")
	}
}

func TestLoadRejectsMalformedOrOpenTaskPolicy(t *testing.T) {
	t.Parallel()
	valid := "workspace:\n  repo: .\ntasks:\n  agent: build\n  model:\n    provider: openai\n    id: gpt-5\n  attemptTimeout: 30m\n  leaseDuration: 2m\n  budget:\n    maxTurns: 100\n"
	tests := map[string]string{
		"null":                     "workspace:\n  repo: .\ntasks: null\n",
		"sequence":                 "workspace:\n  repo: .\ntasks: []\n",
		"agent_type":               strings.Replace(valid, "agent: build", "agent: 1", 1),
		"provider_type":            strings.Replace(valid, "provider: openai", "provider: true", 1),
		"model_id_type":            strings.Replace(valid, "id: gpt-5", "id: [gpt-5]", 1),
		"attempt_type":             strings.Replace(valid, "attemptTimeout: 30m", "attemptTimeout: 30", 1),
		"attempt_invalid":          strings.Replace(valid, "attemptTimeout: 30m", "attemptTimeout: soon", 1),
		"lease_type":               strings.Replace(valid, "leaseDuration: 2m", "leaseDuration: 120", 1),
		"lease_invalid":            strings.Replace(valid, "leaseDuration: 2m", "leaseDuration: later", 1),
		"turns_type":               strings.Replace(valid, "maxTurns: 100", "maxTurns: '100'", 1),
		"turns_zero":               strings.Replace(valid, "maxTurns: 100", "maxTurns: 0", 1),
		"turns_negative":           strings.Replace(valid, "maxTurns: 100", "maxTurns: -1", 1),
		"turns_noncanonical":       strings.Replace(valid, "maxTurns: 100", "maxTurns: 0100", 1),
		"background_image_empty":   valid + "  backgroundImage: ''\n",
		"background_id_empty":      valid + "  backgroundImageID: ''\n",
		"background_image_unknown": valid + "  background_image: image:test\n",
		"background_route_unknown": valid + "  backgroundRoute:\n    listen: 127.0.0.1:9090\n    origin: https://fern.example.ts.net:8443\n    extra: true\n",
		"background_route_type":    valid + "  backgroundRoute: wrong\n",
		"tasks_unknown":            valid + "  unknown: true\n",
		"model_unknown":            strings.Replace(valid, "    id: gpt-5\n", "    id: gpt-5\n    unknown: true\n", 1),
		"budget_unknown":           strings.Replace(valid, "    maxTurns: 100\n", "    maxTurns: 100\n    unknown: true\n", 1),
		"verification_unknown":     valid + "  verification:\n    checkName: tests\n    argv: [/usr/bin/make, test]\n    workingDirectory: ''\n    timeout: 1m\n    environment: {}\n    outputBytes: 4096\n    unknown: true\n",
		"verification_argv_type":   valid + "  verification:\n    checkName: tests\n    argv: /usr/bin/make\n    workingDirectory: ''\n    timeout: 1m\n    environment: {}\n    outputBytes: 4096\n",
		"verification_output_type": valid + "  verification:\n    checkName: tests\n    argv: [/usr/bin/make]\n    workingDirectory: ''\n    timeout: 1m\n    environment: {}\n    outputBytes: '4096'\n",
	}
	for name, data := range tests {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "fern.yaml")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path, filepath.Dir(path), true, Overrides{}); err == nil {
				t.Fatalf("Load accepted malformed task policy:\n%s", data)
			}
		})
	}
}

func TestValidateTaskPolicyBoundsAndDependencies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"github", func(value *Config) { value.Workspace.GitHub = nil }},
		{"installation_zero", func(value *Config) { value.Workspace.GitHub.InstallationID = 0 }},
		{"installation_negative", func(value *Config) { value.Workspace.GitHub.InstallationID = -1 }},
		{"opencode_password", func(value *Config) { delete(value.Workspace.Env, "OPENCODE_PASSWORD") }},
		{"agent_empty", func(value *Config) { value.Tasks.Agent = "" }},
		{"agent_control", func(value *Config) { value.Tasks.Agent = "bad\nagent" }},
		{"agent_too_long", func(value *Config) { value.Tasks.Agent = strings.Repeat("a", 129) }},
		{"agent_invalid_utf8", func(value *Config) { value.Tasks.Agent = string([]byte{0xff}) }},
		{"provider_empty", func(value *Config) { value.Tasks.Model.Provider = "" }},
		{"provider_control", func(value *Config) { value.Tasks.Model.Provider = "bad\x7fprovider" }},
		{"provider_too_long", func(value *Config) { value.Tasks.Model.Provider = strings.Repeat("p", 129) }},
		{"model_empty", func(value *Config) { value.Tasks.Model.ID = "" }},
		{"model_control", func(value *Config) { value.Tasks.Model.ID = "bad\tmodel" }},
		{"model_too_long", func(value *Config) { value.Tasks.Model.ID = strings.Repeat("m", 257) }},
		{"attempt_zero", func(value *Config) { value.Tasks.AttemptTimeout = 0 }},
		{"attempt_short", func(value *Config) { value.Tasks.AttemptTimeout = time.Minute - time.Nanosecond }},
		{"attempt_long", func(value *Config) { value.Tasks.AttemptTimeout = 24*time.Hour + time.Nanosecond }},
		{"lease_zero", func(value *Config) { value.Tasks.LeaseDuration = 0 }},
		{"lease_negative", func(value *Config) { value.Tasks.LeaseDuration = -time.Second }},
		{"lease_long", func(value *Config) { value.Tasks.LeaseDuration = 5*time.Minute + time.Nanosecond }},
		{"lease_after_timeout", func(value *Config) {
			value.Tasks.AttemptTimeout, value.Tasks.LeaseDuration = time.Minute, 2*time.Minute
		}},
		{"turns_zero", func(value *Config) { value.Tasks.Budget.MaxTurns = 0 }},
		{"turns_negative", func(value *Config) { value.Tasks.Budget.MaxTurns = -1 }},
		{"turns_high", func(value *Config) { value.Tasks.Budget.MaxTurns = 1001 }},
		{"background_image_unpaired", func(value *Config) { value.Tasks.BackgroundImage = "image:test" }},
		{"background_id_unpaired", func(value *Config) {
			value.Tasks.BackgroundImageID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{"background_id_short", func(value *Config) {
			value.Tasks.BackgroundImage, value.Tasks.BackgroundImageID = "image:test", "sha256:bbbb"
		}},
		{"background_id_uppercase", func(value *Config) {
			value.Tasks.BackgroundImage, value.Tasks.BackgroundImageID = "image:test", "sha256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := validTaskConfig(t)
			test.mutate(&value)
			if err := Validate(value); err == nil {
				t.Fatal("Validate accepted invalid task policy")
			}
		})
	}
}

func TestValidateTaskPolicyAcceptsBoundaryValues(t *testing.T) {
	t.Parallel()
	minimum := validTaskConfig(t)
	minimum.Tasks.AttemptTimeout = time.Minute
	minimum.Tasks.LeaseDuration = time.Minute
	minimum.Tasks.Budget.MaxTurns = 1
	if err := Validate(minimum); err != nil {
		t.Fatalf("minimum policy: %v", err)
	}
	maximum := validTaskConfig(t)
	maximum.Tasks.AttemptTimeout = 24 * time.Hour
	maximum.Tasks.LeaseDuration = 5 * time.Minute
	maximum.Tasks.Budget.MaxTurns = 1000
	if err := Validate(maximum); err != nil {
		t.Fatalf("maximum policy: %v", err)
	}
}

func TestValidateBackgroundRouteContract(t *testing.T) {
	t.Parallel()
	valid := func(t *testing.T) Config {
		value := validTaskConfig(t)
		value.RemoteOrigin = "https://fern.example.ts.net"
		value.Tasks.BackgroundImage = "image:test"
		value.Tasks.BackgroundImageID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		value.Tasks.BackgroundRoute = &BackgroundRoute{Listen: "127.0.0.1:9090", Origin: "https://fern.example.ts.net:8443"}
		return value
	}
	if err := Validate(valid(t)); err != nil {
		t.Fatalf("valid background route: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing", func(value *Config) { value.Tasks.BackgroundRoute = nil }},
		{"without image", func(value *Config) { value.Tasks.BackgroundImage, value.Tasks.BackgroundImageID = "", "" }},
		{"non-loopback", func(value *Config) { value.Tasks.BackgroundRoute.Listen = "0.0.0.0:9090" }},
		{"remote missing", func(value *Config) { value.RemoteOrigin = "" }},
		{"hostname mismatch", func(value *Config) { value.Tasks.BackgroundRoute.Origin = "https://other.example.ts.net:8443" }},
		{"localhost origin", func(value *Config) {
			value.RemoteOrigin = "https://localhost"
			value.Tasks.BackgroundRoute.Origin = "https://localhost:8443"
		}},
		{"single-label origin", func(value *Config) {
			value.RemoteOrigin = "https://fern"
			value.Tasks.BackgroundRoute.Origin = "https://fern:8443"
		}},
		{"origin port missing", func(value *Config) { value.Tasks.BackgroundRoute.Origin = "https://fern.example.ts.net" }},
		{"origin port 443", func(value *Config) { value.Tasks.BackgroundRoute.Origin = "https://fern.example.ts.net:443" }},
		{"same remote port", func(value *Config) {
			value.RemoteOrigin = "https://fern.example.ts.net:8443"
			value.Tasks.BackgroundRoute.Origin = "https://fern.example.ts.net:8443"
		}},
		{"remote listener collision", func(value *Config) { value.Tasks.BackgroundRoute.Listen = value.Listen }},
		{"operator listener collision", func(value *Config) { value.Tasks.BackgroundRoute.Listen = value.OperatorListen }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := valid(t)
			test.mutate(&value)
			if err := Validate(value); err == nil {
				t.Fatal("invalid background route was accepted")
			}
		})
	}
}

func TestTaskConfigurationIsOptionalAndPublicationCompatible(t *testing.T) {
	t.Parallel()
	if value := Default(t.TempDir()); value.Tasks != nil {
		t.Fatalf("default tasks = %+v", value.Tasks)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := "workspace:\n  repo: .\n  env:\n    OPENCODE_PASSWORD: opencode-secret\n  github:\n    mode: workspace-gh\n    hostname: github.com\n    repository:\n      id: 123\n      fullName: owner/repository\ncontrol:\n  password: control-secret-control-secret-1234\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, directory, true, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tasks != nil || loaded.Workspace.GitHub == nil || loaded.Workspace.GitHub.InstallationID != 0 {
		t.Fatalf("optional task configuration = %+v, GitHub = %+v", loaded.Tasks, loaded.Workspace.GitHub)
	}
	if err := Validate(loaded); err != nil {
		t.Fatalf("publication-only configuration: %v", err)
	}
}

func TestLoadRejectsInvalidOptionalInstallationID(t *testing.T) {
	t.Parallel()
	for _, installationID := range []string{"0", "-1", "+1", "01", "1.0", "'1'", "9223372036854775808"} {
		installationID := installationID
		t.Run(installationID, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "fern.yaml")
			data := "workspace:\n  repo: .\n  github:\n    mode: github-app-broker\n    hostname: github.com\n    installationId: " + installationID + "\n    repository:\n      id: 123\n      fullName: owner/repository\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path, filepath.Dir(path), true, Overrides{}); err == nil {
				t.Fatalf("Load accepted installation ID %q", installationID)
			}
		})
	}
}

func TestTaskPolicyDoesNotExpandEnvironmentOrCaptureSecrets(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := `workspace:
  repo: .
  env:
    OPENCODE_PASSWORD: ${OPENCODE_PASSWORD}
  github:
    mode: github-app-broker
    hostname: github.com
    installationId: 1
    repository:
      id: 2
      fullName: owner/repository
control:
  password: ${FERN_CONTROL_PASSWORD}
tasks:
  agent: ${TASK_AGENT}
  model:
    provider: ${TASK_PROVIDER}
    id: ${TASK_MODEL}
  attemptTimeout: 30m
  leaseDuration: 2m
  budget:
    maxTurns: 100
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithEnvironment(path, directory, true, Overrides{}, map[string]string{
		"OPENCODE_PASSWORD":     "opencode-secret",
		"FERN_CONTROL_PASSWORD": "control-secret-control-secret-1234",
		"TASK_AGENT":            "secret-agent", "TASK_PROVIDER": "paid-provider", "TASK_MODEL": "paid-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tasks.Agent != "${TASK_AGENT}" || loaded.Tasks.Model.Provider != "${TASK_PROVIDER}" || loaded.Tasks.Model.ID != "${TASK_MODEL}" {
		t.Fatalf("task policy was environment-expanded: %+v", loaded.Tasks)
	}
	if loaded.Workspace.Env["OPENCODE_PASSWORD"] != "opencode-secret" || loaded.Control.Password != "control-secret-control-secret-1234" {
		t.Fatal("credential fields were not expanded")
	}
}

func TestBackgroundEnvironmentIsExplicitExpandedAndSeparated(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	data := `workspace:
  repo: .
  env:
    OPENCODE_PASSWORD: ${OPENCODE_PASSWORD}
    WORKSPACE_PROVIDER_KEY: workspace-only
  github:
    mode: github-app-broker
    hostname: github.com
    installationId: 1
    repository:
      id: 2
      fullName: owner/repository
control:
  password: ${FERN_CONTROL_PASSWORD}
tasks:
  agent: build
  model:
    provider: openai
    id: gpt-5
  attemptTimeout: 30m
  leaseDuration: 2m
  backgroundEnvironment:
    OPENAI_API_KEY: ${OPENAI_API_KEY}
  budget:
    maxTurns: 100
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithEnvironment(path, directory, true, Overrides{}, map[string]string{
		"OPENCODE_PASSWORD": "workspace-password", "FERN_CONTROL_PASSWORD": "control-password-control-password-1234",
		"OPENAI_API_KEY": "explicit-provider-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"OPENAI_API_KEY": "explicit-provider-key"}
	if !reflect.DeepEqual(loaded.Tasks.BackgroundEnvironment, want) {
		t.Fatalf("background environment = %#v, want %#v", loaded.Tasks.BackgroundEnvironment, want)
	}
	if _, exists := loaded.Tasks.BackgroundEnvironment["WORKSPACE_PROVIDER_KEY"]; exists {
		t.Fatal("workspace environment crossed into disposable background environment")
	}
	if err := Validate(loaded); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundEnvironmentRejectsCredentialCustodyViolations(t *testing.T) {
	base := Default(t.TempDir())
	base.Workspace.Env["OPENCODE_PASSWORD"] = "workspace-password"
	base.Control.Password = "control-password-control-password-1234"
	base.Tasks = &TaskPolicy{Agent: "build", Model: TaskModel{Provider: "openai", ID: "gpt-5"}, AttemptTimeout: time.Hour,
		LeaseDuration: time.Minute, Budget: TaskBudget{MaxTurns: 10}}
	tests := map[string]map[string]string{
		"legacy password":     {"OPENCODE_PASSWORD": "other"},
		"server password":     {"OPENCODE_SERVER_PASSWORD": "other"},
		"server username":     {"OPENCODE_SERVER_USERNAME": "other"},
		"Fern credential":     {"FERN_CONTROL_PASSWORD": "other"},
		"GitHub credential":   {"GITHUB_TOKEN": "other"},
		"workspace embedding": {"MODEL_KEY": "prefix-workspace-password-suffix"},
		"control embedding":   {"MODEL_KEY": "prefix-control-password-control-password-1234-suffix"},
		"invalid name":        {"BAD-NAME": "value"},
		"NUL value":           {"MODEL_KEY": "bad\x00value"},
	}
	for name, environment := range tests {
		t.Run(name, func(t *testing.T) {
			value := base
			policy := *base.Tasks
			policy.BackgroundEnvironment = environment
			value.Tasks = &policy
			if err := Validate(value); err == nil {
				t.Fatal("unsafe background environment was accepted")
			}
		})
	}
}

func TestExamplesRemainLoadableWithTasksDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path        string
		defaultRepo string
	}{
		{"../../fern.example.yaml", "../.."},
		{"../../deploy/systemd/fern.yaml.example", "../.."},
	}
	for _, test := range tests {
		test := test
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			loaded, err := LoadWithEnvironment(test.path, test.defaultRepo, true, Overrides{}, map[string]string{
				"OPENCODE_PASSWORD":     "opencode-secret",
				"FERN_CONTROL_PASSWORD": "control-secret-control-secret-1234",
			})
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Tasks != nil {
				t.Fatalf("example unexpectedly enables tasks: %+v", loaded.Tasks)
			}
		})
	}
}

func validTaskConfig(t *testing.T) Config {
	t.Helper()
	value := Default(t.TempDir())
	value.Workspace.Env["OPENCODE_PASSWORD"] = "opencode-secret"
	value.Control.Password = "control-secret-control-secret-1234"
	value.Workspace.GitHub = &WorkspaceGitHub{
		Mode:           GitHubModeGitHubAppBroker,
		Hostname:       "github.com",
		InstallationID: 123,
		Repository:     GitHubRepository{ID: 456, FullName: "owner/repository"},
	}
	value.Tasks = &TaskPolicy{
		Agent: "build", Model: TaskModel{Provider: "openai", ID: "gpt-5"},
		AttemptTimeout: 30 * time.Minute, LeaseDuration: 2 * time.Minute,
		Budget: TaskBudget{MaxTurns: 100},
	}
	return value
}

func TestGitHubAuthorityModesAreExplicitAndClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		github  string
		wantErr bool
	}{
		{name: "workspace gh", github: "mode: workspace-gh\n    hostname: github.com\n", wantErr: false},
		{name: "broker", github: "mode: github-app-broker\n    hostname: github.com\n    installationId: 7\n", wantErr: false},
		{name: "implicit", github: "hostname: github.com\n", wantErr: true},
		{name: "workspace gh installation", github: "mode: workspace-gh\n    hostname: github.com\n    installationId: 7\n", wantErr: true},
		{name: "broker missing installation", github: "mode: github-app-broker\n    hostname: github.com\n", wantErr: true},
		{name: "other host", github: "mode: workspace-gh\n    hostname: example.com\n", wantErr: true},
		{name: "unknown mode", github: "mode: other\n    hostname: github.com\n", wantErr: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "fern.yaml")
			data := "workspace:\n  repo: .\n  github:\n    " + test.github + "    repository:\n      id: 123\n      fullName: owner/repository\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(path, filepath.Dir(path), true, Overrides{})
			if test.wantErr {
				if err == nil {
					t.Fatalf("Load accepted %s: %+v", test.name, loaded.Workspace.GitHub)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkspaceGHTasksDoNotRequireInstallationID(t *testing.T) {
	config := validTaskConfig(t)
	config.Workspace.GitHub.Mode = GitHubModeWorkspaceGH
	config.Workspace.GitHub.InstallationID = 0
	if err := Validate(config); err != nil {
		t.Fatalf("workspace-gh task configuration: %v", err)
	}
	config.Workspace.GitHub.InstallationID = 7
	if err := Validate(config); err == nil {
		t.Fatal("workspace-gh accepted an App installation ID")
	}
}

func TestWorkspaceGHConfigDirectoryIsFernManaged(t *testing.T) {
	config := validTaskConfig(t)
	config.Workspace.GitHub.Mode = GitHubModeWorkspaceGH
	config.Workspace.GitHub.InstallationID = 0
	config.Workspace.Env["GH_CONFIG_DIR"] = "/tmp/gh"
	if err := Validate(config); err == nil {
		t.Fatal("configuration accepted caller-managed GH_CONFIG_DIR")
	}
}
