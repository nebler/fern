package main

import (
	"net"
	"testing"

	"github.com/nebler/fern/internal/config"
)

func TestMissingGitHubBindingDoesNotResolveGitHubCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	workspace := config.Default(t.TempDir()).Workspace
	publisher, err := newWorkspacePublisher(workspace)
	if err != nil || publisher != nil {
		t.Fatalf("publisher=%v err=%v", publisher, err)
	}
}

func TestGitHubAppBindingDoesNotResolveLegacyGitHubCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	workspace := config.Default(t.TempDir()).Workspace
	workspace.GitHub = &config.WorkspaceGitHub{
		Mode:           config.GitHubModeGitHubAppBroker,
		Hostname:       "github.com",
		InstallationID: 7,
		Repository:     config.GitHubRepository{ID: 123, FullName: "owner/repo"},
	}
	publisher, err := newWorkspacePublisher(workspace)
	if err != nil || publisher != nil {
		t.Fatalf("publisher=%v err=%v", publisher, err)
	}
}

func TestTrustedProxyOriginsPreserveLocalCompatibility(t *testing.T) {
	t.Parallel()
	cfg := config.Default(t.TempDir())
	origins := trustedProxyOrigins(cfg)
	if origins.Remote != "http://127.0.0.1:8080" || origins.Operator != "http://127.0.0.1:8081" {
		t.Fatalf("local origins = %+v", origins)
	}
	cfg.RemoteOrigin = "https://fern.example.ts.net"
	origins = trustedProxyOrigins(cfg)
	if origins.Remote != cfg.RemoteOrigin || origins.Operator != "http://127.0.0.1:8081" {
		t.Fatalf("published origins = %+v", origins)
	}
}

func TestListenProxySurfacesReleasesRemoteWhenOperatorBindFails(t *testing.T) {
	t.Parallel()
	operator, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remoteAddress := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenProxySurfaces(remoteAddress, operator.Addr().String()); err == nil {
		t.Fatal("listener setup succeeded with occupied operator address")
	}
	rebound, err := net.Listen("tcp", remoteAddress)
	if err != nil {
		t.Fatalf("remote listener remained bound after operator failure: %v", err)
	}
	_ = rebound.Close()
}
