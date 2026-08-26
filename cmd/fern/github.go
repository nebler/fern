package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/publication"
	fernRuntime "github.com/nebler/fern/internal/runtime"
)

func runGitHubPublish(args []string, log *slog.Logger) error {
	fs := newFlagSet("github publish", "Push committed work to a Fern branch and open a draft pull request.")
	configPath := fs.String("config", "fern.yaml", "configuration file")
	operation := fs.String("operation", "", "publication identifier (defaults to the commit prefix)")
	base := fs.String("base", "", "base branch (defaults to repository default)")
	title := fs.String("title", "", "draft pull request title")
	body := fs.String("body", "Created from a private Fern workspace.", "draft pull request body")
	dryRun := fs.Bool("dry-run", false, "validate without push or pull request creation")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if !*dryRun {
		return invocationError{message: "standalone publication cannot safely provide durable persistence for GitHub effects; use the publication control on a configured running 'fern up' service"}
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
	if workspace.Workspace.GitHub == nil {
		return errors.New("GitHub publication is disabled; configure workspace.github.repository.id and fullName")
	}
	publisher, err := publication.New(workspace.Workspace.Name, workspace.Workspace.Repo, publication.RepositoryBinding{
		ID: workspace.Workspace.GitHub.Repository.ID, FullName: workspace.Workspace.GitHub.Repository.FullName,
	})
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
	fmt.Printf("GitHub publication preflight passed\nrepository: %s (%d)\nbase: %s at %s\ncommit: %s\nbranch: %s\n", prepared.RepositoryFullName, prepared.RepositoryID, prepared.BaseRef, prepared.BaseSHA, prepared.ResultCommit, prepared.Branch)
	return nil
}
