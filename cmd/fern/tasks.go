package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/backgroundruncoord"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/runapi"
	"github.com/nebler/fern/internal/runclientapi"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskresultsource"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	taskServiceCredentialID       = "service-v1"
	taskPollInterval              = time.Second
	taskOperationTimeout          = 2 * time.Minute
	taskInspectTimeout            = 15 * time.Second
	backgroundCloneTimeout        = 30 * time.Second
	backgroundCloneAdmissionBytes = 128 << 20
)

type taskRunService interface {
	Run(context.Context) error
}

type taskWakeService interface {
	taskRunService
	Wake()
}

type taskServices struct {
	store      *taskstore.Store
	runs       http.Handler
	runClients http.Handler
	background taskWakeService
	provider   *taskenvdocker.Provider
	artifact   *taskartifact.Engine
	status     *observability.Registry
}

func (services *taskServices) Close() error {
	return errors.Join(services.artifact.Close(), services.provider.Close(), services.store.Close())
}

func newTaskServices(ctx context.Context, cfg config.Config, route *backgroundroute.Manager, status *observability.Registry, log *slog.Logger) (*taskServices, error) {
	if cfg.Tasks.BackgroundImage == "" || cfg.Tasks.BackgroundImageID == "" || route == nil {
		return nil, errors.New("a qualified disposable Background Run profile is required")
	}
	github := cfg.Workspace.GitHub
	taskDirectory, err := statePath("tasks")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(taskDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create task state directory: %w", err)
	}
	store, err := taskstore.Open(ctx, filepath.Join(taskDirectory, cfg.Workspace.Name+".db"))
	if err != nil {
		return nil, err
	}
	closeStore := true
	defer func() {
		if closeStore {
			_ = store.Close()
		}
	}()

	ids := task.NewSecureGenerator()
	authority, err := resolveGitHubAuthority(github)
	if err != nil {
		return nil, err
	}
	status.Healthy(observability.ComponentGitHubTaskDependency)

	backgroundRoot := filepath.Join(taskDirectory, cfg.Workspace.Name+"-background")
	if err := os.MkdirAll(backgroundRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create background run state root: %w", err)
	}
	backgroundRoot, err = filepath.EvalSymlinks(backgroundRoot)
	if err != nil {
		return nil, err
	}
	providerRoot := filepath.Join(backgroundRoot, "runtime")
	casRoot := filepath.Join(backgroundRoot, "artifact-cas")
	workRoot := filepath.Join(backgroundRoot, "artifact-work")
	for _, root := range []string{providerRoot, casRoot, workRoot} {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create background state root: %w", err)
		}
	}
	artifact, err := taskartifact.New(taskartifact.Config{GitExecutable: gitExecutable(), CASRoot: casRoot,
		WorkRoot: workRoot, CommandTimeout: taskOperationTimeout})
	if err != nil {
		return nil, err
	}
	closeArtifact := true
	defer func() {
		if closeArtifact {
			_ = artifact.Close()
		}
	}()
	referencedArtifacts, err := store.ReferencedArtifactManifestSHA256(ctx)
	if err != nil {
		return nil, fmt.Errorf("list retained artifacts: %w", err)
	}
	for _, digest := range referencedArtifacts {
		locator, parseErr := taskartifact.ParseLocator("sha256:" + hex.EncodeToString(digest[:]))
		if parseErr != nil {
			return nil, parseErr
		}
		if _, inspectErr := artifact.Inspect(ctx, locator); inspectErr != nil {
			return nil, fmt.Errorf("reconcile retained artifact: %w", inspectErr)
		}
	}

	candidateID, err := ids.WorkspaceID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	desired := taskstore.Workspace{ID: candidateID, Name: cfg.Workspace.Name, State: taskstore.WorkspaceActive,
		RepositoryPath: cfg.Workspace.Repo, GitHubAuthority: taskstore.GitHubAuthorityAppBroker,
		InstallationID: task.InstallationID(github.InstallationID), RepositoryID: task.RepositoryID(github.Repository.ID),
		RepositoryFullName: github.Repository.FullName, ImageDigest: cfg.Tasks.BackgroundImageID,
		OpenCodeProtocol: runapi.APIContractVersion, RuntimeDesiredState: "disposable", ReconciliationEpoch: 1, CreatedAt: now}
	if existing, readErr := store.GetWorkspaceByName(ctx, cfg.Workspace.Name); readErr == nil {
		// Preserve the durable workspace identity while still checking every
		// repository and GitHub authority field through EnsureWorkspace.
		desired.ID, desired.ImageDigest, desired.OpenCodeProtocol = existing.ID, existing.ImageDigest, existing.OpenCodeProtocol
		desired.RuntimeDesiredState, desired.ReconciliationEpoch, desired.CreatedAt = existing.RuntimeDesiredState, existing.ReconciliationEpoch, existing.CreatedAt
	} else if !errors.Is(readErr, taskstore.ErrNotFound) {
		return nil, readErr
	}
	durableWorkspace, err := store.EnsureWorkspace(ctx, desired)
	if err != nil {
		return nil, err
	}
	githubIdentity, err := githubapp.NewRepositoryIdentity(int64(durableWorkspace.InstallationID), int64(durableWorkspace.RepositoryID))
	if err != nil {
		return nil, err
	}
	repository, err := filepath.EvalSymlinks(cfg.Workspace.Repo)
	if err != nil {
		return nil, fmt.Errorf("resolve Background Run repository: %w", err)
	}
	provider, err := taskenvdocker.New(ctx, taskenvdocker.Config{
		RuntimeStorageRoot: cfg.Tasks.RuntimeStorageRoot,
		StateRoot:          providerRoot, Repository: repository, GitExecutable: gitExecutable(),
		GitHubTokens: authority.installationTokens, GitHubRepository: githubIdentity,
		GitHubRepositoryFullName: durableWorkspace.RepositoryFullName,
		ImageReference:           cfg.Tasks.BackgroundImage, ImageID: cfg.Tasks.BackgroundImageID, MemoryBytes: 1 << 30,
		WallTimeout: 24 * time.Hour, GitTimeout: backgroundCloneTimeout,
		DockerTimeout: 30 * time.Second, HealthTimeout: 30 * time.Second, GitOutputBytes: 1 << 20,
		SourceSizeAdmissionBytes: backgroundCloneAdmissionBytes, CloneObservedLimitBytes: backgroundCloneAdmissionBytes, DiskFreeAdmissionBytes: 20 << 30,
		LogMaxSize: "10m", LogMaxFiles: 3, StopGrace: 10 * time.Second,
	}, nil)
	if err != nil {
		return nil, err
	}
	closeProvider := true
	defer func() {
		if closeProvider {
			_ = provider.Close()
		}
	}()

	resultSource, err := taskresultsource.New(store, artifact)
	if err != nil {
		return nil, err
	}

	coordinator, err := backgroundruncoord.New(store, provider, artifact, ids, backgroundruncoord.Config{
		WorkspaceID: durableWorkspace.ID, SystemActor: systemActor("background-run", "Background Run coordinator"),
		Profile: runapi.PluginOpenCodeProfile, ImageIdentity: cfg.Tasks.BackgroundImageID,
		EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), Agent: cfg.Tasks.Agent,
		ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID,
		OperationTimeout: backgroundCloneTimeout,
		PollInterval:     taskPollInterval, HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 100, MaxPages: 100, MaxEvents: 10000},
		Now: time.Now, HTTPClient: &http.Client{Timeout: backgroundCloneTimeout}, Route: route,
		OnError: func(err error) {
			status.Degraded(observability.ComponentBackgroundRunSerial, err)
			log.Error("Background Run coordination deferred", "err", err, "repository", cfg.Workspace.Name)
		},
		OnSuccess: func() { status.Healthy(observability.ComponentBackgroundRunSerial) },
	})
	if err != nil {
		return nil, err
	}
	baseVerifier, err := runapi.NewGitBaseVerifier(cfg.Workspace.Repo, gitExecutable(), taskInspectTimeout)
	if err != nil {
		return nil, err
	}
	runs, err := runapi.New(runapi.Config{
		WorkspaceID: durableWorkspace.ID, RepositoryID: durableWorkspace.RepositoryID,
		RepositoryRemote:            "https://github.com/" + github.Repository.FullName,
		BackgroundImageIdentity:     cfg.Tasks.BackgroundImageID,
		BackgroundEnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil),
		AvailableProfile:            runapi.PluginOpenCodeProfile, Store: store, Route: route, Generator: ids, ActorResolver: task.ContextActor,
		BaseVerifier: baseVerifier, Now: time.Now, AttemptTimeout: cfg.Tasks.AttemptTimeout, Agent: cfg.Tasks.Agent,
		ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID, RetentionVerifier: resultSource,
		SealPolicyVersion: "fern.background-user-seal.v1", Wake: coordinator.Wake,
	})
	if err != nil {
		return nil, err
	}
	runClients, err := runclientapi.New(runclientapi.Config{WorkspaceID: durableWorkspace.ID, Store: store, Route: route})
	if err != nil {
		return nil, err
	}
	status.Qualified(observability.ComponentBackgroundRunProfile)
	status.Healthy(observability.ComponentBackgroundRunSerial)
	closeStore, closeArtifact, closeProvider = false, false, false
	return &taskServices{store: store, runs: runs, runClients: runClients,
		background: coordinator, provider: provider, artifact: artifact, status: status}, nil
}

type gitHubAuthority struct {
	installationTokens githubapp.InstallationTokenSource
}

func resolveGitHubAuthority(github config.GitHubApp) (*gitHubAuthority, error) {
	directory, err := statePath("github-app")
	if err != nil {
		return nil, err
	}
	credentialStore, err := githubapp.NewCredentialStore(directory)
	if err != nil {
		return nil, err
	}
	credentials, err := credentialStore.Load()
	if err != nil {
		return nil, fmt.Errorf("load GitHub App credentials: %w", err)
	}
	signer, err := githubapp.NewJWTSigner(credentials.AppID(), credentials.PrivateKey())
	if err != nil {
		return nil, err
	}
	tokens, err := githubapp.NewClient(http.DefaultClient, signer)
	if err != nil {
		return nil, err
	}
	if _, err := githubapp.NewRepositoryIdentity(github.InstallationID, github.Repository.ID); err != nil {
		return nil, err
	}
	return &gitHubAuthority{installationTokens: tokens}, nil
}

func systemActor(id, displayName string) task.ActorSnapshot {
	return task.ActorSnapshot{Type: task.ActorSystem, ID: id, DisplayName: displayName,
		CredentialID: taskServiceCredentialID, Authentication: "internal", RequestID: id}
}

func gitExecutable() string {
	if goruntime.GOOS == "darwin" {
		for _, candidate := range []string{"/Library/Developer/CommandLineTools/usr/bin/git", "/Applications/Xcode.app/Contents/Developer/usr/bin/git"} {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return candidate
			}
		}
	}
	return "/usr/bin/git"
}

func newGitHubOnboarding(cfg config.Config) (http.Handler, error) {
	if cfg.RemoteOrigin == "" {
		return nil, nil
	}
	directory, err := statePath("github-app")
	if err != nil {
		return nil, err
	}
	credentials, err := githubapp.NewCredentialStore(directory)
	if err != nil {
		return nil, err
	}
	if _, err := credentials.Load(); err == nil {
		return nil, nil
	} else if !errors.Is(err, githubapp.ErrCredentialsNotFound) {
		return nil, err
	}
	states, err := githubapp.NewOnboardingStateStore(filepath.Join(directory, "onboarding"))
	if err != nil {
		return nil, err
	}
	exchanger, err := githubapp.NewManifestClient(http.DefaultClient)
	if err != nil {
		return nil, err
	}
	return githubapp.NewOnboardingHTTPWithSetupOrigin(cfg.RemoteOrigin, "http://"+cfg.OperatorListen,
		"Fern "+cfg.Workspace.Name, states, exchanger, credentials, rand.Reader, time.Now)
}
