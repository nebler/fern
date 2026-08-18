package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/publication"
	fernRuntime "github.com/nebler/fern/internal/runtime"
)

func runGitHub(args []string, log *slog.Logger) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stdout, "Publish committed work through host GitHub credentials.\n\nUsage:\n  fern github publish [flags]")
		return nil
	}
	if len(args) == 0 || args[0] != "publish" {
		return unknownCommand(append([]string{"github"}, args...))
	}
	return runGitHubPublish(args[1:], log)
}

func runGitHubPublish(args []string, log *slog.Logger) error {
	flags := newFlagSet("github publish", "Push committed work to a Fern branch and open a draft pull request.")
	configPath := flags.String("config", "fern.yaml", "configuration file")
	operation := flags.String("operation", "", "publication identifier (defaults to the commit prefix)")
	base := flags.String("base", "", "base branch (defaults to repository default)")
	title := flags.String("title", "", "draft pull request title")
	body := flags.String("body", "Created from a private Fern workspace.", "draft pull request body")
	dryRun := flags.Bool("dry-run", false, "validate without push or pull request creation")
	if err := parseFlags(flags, args); err != nil {
		return err
	}
	if strings.TrimSpace(*title) == "" || len(*title) > 256 {
		return invocationError{message: "--title is required and must be at most 256 bytes"}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	workspace, err := config.LoadWorkspace(*configPath, cwd, true, config.Overrides{})
	if err != nil {
		return err
	}
	lease, err := acquireWorkspaceLease(workspace.Workspace.Name)
	if err != nil {
		return fmt.Errorf("publication requires exclusive workspace access: %w", err)
	}
	defer lease.Release()
	docker, err := newDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	statusCtx, statusCancel := context.WithTimeout(context.Background(), 10*time.Second)
	observation, err := docker.Status(statusCtx, workspace.Workspace.Name)
	statusCancel()
	if err != nil {
		return fmt.Errorf("inspect workspace before publication: %w", err)
	}
	if observation.State != fernRuntime.StateAbsent {
		return fmt.Errorf("publication requires removed compute; stop the service and run 'fern down' first (state: %s)", observation.State)
	}
	publisher, err := publication.New(workspace.Workspace.Name, workspace.Workspace.Repo)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	request := publication.Request{Operation: *operation, Base: *base, Title: *title, Body: *body}
	prepared, err := publisher.Prepare(ctx, request)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("GitHub publication preflight passed\nrepository: %s\nbase: %s\ncommit: %s\nbranch: %s\n", prepared.Repository, prepared.Base, prepared.Commit, prepared.Branch)
		return nil
	}
	result, err := publisher.PublishPrepared(ctx, prepared, request.Title, request.Body)
	if err != nil {
		return err
	}
	fmt.Printf("Draft pull request ready\nrepository: %s\nbranch: %s\ncommit: %s\nurl: %s\n", result.Repository, result.Branch, result.Commit, result.URL)
	return nil
}

type publishRepository = publication.Repository

func inspectPublishRepository(path string) (publishRepository, error) {
	return publication.InspectRepository(context.Background(), path)
}

func validateLocalGitConfig(config string) error {
	return publication.ValidateLocalGitConfig(config)
}

func githubRepositoryName(remote string) (string, error) {
	return publication.GitHubRepositoryName(remote)
}
