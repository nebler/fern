package config

import (
	"strings"
	"testing"
)

func TestBackgroundProjectionDropsLegacyRuntimeConfiguration(t *testing.T) {
	t.Parallel()
	legacy := validTaskConfig(t)
	legacy.RemoteOrigin = "https://fern.example.ts.net"
	legacy.Tasks.BackgroundImage = "image:test"
	legacy.Tasks.BackgroundImageID = "sha256:" + strings.Repeat("b", 64)
	legacy.Tasks.BackgroundRoute = &BackgroundRoute{Listen: "127.0.0.1:9090", Origin: "https://fern.example.ts.net:8443"}
	legacy.Tasks.BackgroundEnvironment = map[string]string{}
	legacy.Workspace.Image = "legacy-image-sentinel"
	legacy.Workspace.Memory = "not-a-runtime-memory-value"
	legacy.Workspace.Env["LEGACY_SENTINEL"] = "must-not-project"
	legacy.IdleAfter = -1
	legacy.IdleMode = "retired-mode"

	projected, err := ProjectBackgroundBootstrap(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Workspace.Name != legacy.Workspace.Name || projected.Workspace.Repo != legacy.Workspace.Repo {
		t.Fatalf("projected workspace = %+v", projected.Workspace)
	}
	legacy.Tasks.BackgroundEnvironment["AFTER"] = "legacy"
	projected.Tasks.BackgroundEnvironment["PROJECTED"] = "value"
	if _, exists := projected.Tasks.BackgroundEnvironment["AFTER"]; exists {
		t.Fatal("projected background environment aliases legacy configuration")
	}
	if _, exists := legacy.Tasks.BackgroundEnvironment["PROJECTED"]; exists {
		t.Fatal("legacy background environment aliases production projection")
	}
}

func TestBackgroundProjectionRejectsInjectedEnvironment(t *testing.T) {
	t.Parallel()
	legacy := validTaskConfig(t)
	legacy.Tasks.BackgroundEnvironment = map[string]string{"OPENAI_API_KEY": "provider-secret"}
	if _, err := ProjectBackgroundBootstrap(legacy); err == nil || !strings.Contains(err.Error(), "brokered egress") {
		t.Fatalf("injected environment error = %v", err)
	}
}

func TestBackgroundProjectionRejectsRetiredGitHubAuthority(t *testing.T) {
	t.Parallel()
	legacy := validTaskConfig(t)
	legacy.Workspace.GitHub.Mode = GitHubModeWorkspaceGH
	legacy.Workspace.GitHub.InstallationID = 0
	if _, err := ProjectBackgroundBootstrap(legacy); err == nil {
		t.Fatal("workspace-gh projected into production configuration")
	}
}

func TestBackgroundProjectionRejectsOpenCodeCredentialAliases(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "workspace password", value: "legacy-opencode-secret"},
		{name: "environment reference", value: "${OPENCODE_PASSWORD}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			legacy := validTaskConfig(t)
			legacy.Workspace.Env["OPENCODE_PASSWORD"] = "legacy-opencode-secret"
			legacy.Tasks.BackgroundEnvironment = make(map[string]string)
			legacy.Tasks.BackgroundEnvironment["MODEL_SECRET"] = test.value
			if _, err := ProjectBackgroundBootstrap(legacy); err == nil {
				t.Fatal("OpenCode credential projected under an alias")
			}
		})
	}
}
