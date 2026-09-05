package main

import (
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestCredentialBindingUsesExactConfiguredGitHubIdentity(t *testing.T) {
	t.Parallel()
	var cfg config.BackgroundConfig
	if _, err := credentialBinding(cfg); err == nil {
		t.Fatal("credential binding accepted missing GitHub authority")
	}
	cfg.Workspace.Name = "repository-a"
	cfg.Workspace.GitHub = config.BackgroundGitHubApp{InstallationID: 123,
		Repository: config.GitHubRepository{ID: 456, FullName: "owner/repository"},
	}
	binding, err := credentialBinding(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Workspace != "repository-a" || binding.Mode != string(config.GitHubModeGitHubAppBroker) ||
		binding.Hostname != "github.com" || binding.InstallationID != 123 || binding.RepositoryID != 456 ||
		binding.Repository != "owner/repository" {
		t.Fatalf("credential binding = %+v", binding)
	}
}
