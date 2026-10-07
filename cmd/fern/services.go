package main

import (
	"context"
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
	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/runapi"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskresultsource"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	runPollInterval               = time.Second
	runOperationTimeout           = 2 * time.Minute
	runInspectTimeout             = 15 * time.Second
	backgroundCloneTimeout        = 30 * time.Second
	backgroundCloneAdmissionBytes = 128 << 20
)

type runService interface {
	Run(context.Context) error
}

type wakeService interface {
	runService
	Wake()
}

type runServices struct {
	store      *taskstore.Store
	runs       http.Handler
	background wakeService
	provider   *taskenvdocker.Provider
	artifact   *taskartifact.Engine
	status     *observability.Registry
}

// Close releases the artifact engine and Docker provider. The store belongs to
// the caller, which closes it after these.
func (services *runServices) Close() error {
	return errors.Join(services.artifact.Close(), services.provider.Close())
}

// openStateStore opens the workspace's single durable SQLite database.
func openStateStore(ctx context.Context, cfg config.Config) (*taskstore.Store, error) {
	runDirectory, err := statePath("runs")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(runDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create run state directory: %w", err)
	}
	return taskstore.Open(ctx, filepath.Join(runDirectory, cfg.Workspace.Name+".db"))
}

func newRunServices(ctx context.Context, cfg config.Config, store *taskstore.Store, route *backgroundroute.Manager, status *observability.Registry, log *slog.Logger) (*runServices, error) {
	if cfg.Runs.BackgroundImage == "" || cfg.Runs.BackgroundImageID == "" || route == nil {
		return nil, errors.New("a qualified disposable Background Run profile is required")
	}
	github := cfg.Workspace.GitHub
	ids := domain.NewSecureGenerator()
	installationTokens, err := resolveGitHubAuthority(github)
	if err != nil {
		return nil, err
	}
	status.Healthy(observability.ComponentGitHubDependency)

	roots, err := prepareStateRoots(cfg.Workspace.Name)
	if err != nil {
		return nil, err
	}
	artifact, err := taskartifact.New(taskartifact.Config{GitExecutable: gitExecutable(), CASRoot: roots.cas,
		WorkRoot: roots.work, CommandTimeout: runOperationTimeout})
	if err != nil {
		return nil, err
	}
	closeArtifact := true
	defer func() {
		if closeArtifact {
			_ = artifact.Close()
		}
	}()
	if err := inspectRetainedArtifacts(ctx, store, artifact); err != nil {
		return nil, err
	}
	durableWorkspace, err := ensureWorkspace(ctx, cfg, store, ids)
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
		RuntimeStorageRoot: cfg.Runs.RuntimeStorageRoot,
		StateRoot:          roots.provider, Repository: repository, GitExecutable: gitExecutable(),
		GitHubTokens: installationTokens, GitHubRepository: githubIdentity,
		GitHubRepositoryFullName: durableWorkspace.RepositoryFullName,
		ImageReference:           cfg.Runs.BackgroundImage, ImageID: cfg.Runs.BackgroundImageID, MemoryBytes: 1 << 30,
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

	resultSource, err := taskresultsource.New(artifact)
	if err != nil {
		return nil, err
	}

	coordinator, err := backgroundruncoord.New(store, provider, artifact, ids, backgroundruncoord.Config{
		WorkspaceID: durableWorkspace.ID,
		Profile:     domain.SourceProfile, ImageIdentity: cfg.Runs.BackgroundImageID,
		EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), Agent: cfg.Runs.Agent,
		ModelProvider: cfg.Runs.Model.Provider, Model: cfg.Runs.Model.ID,
		OperationTimeout: backgroundCloneTimeout,
		PollInterval:     runPollInterval, HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 100, MaxPages: 100, MaxEvents: 10000},
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
	baseVerifier, err := runapi.NewGitBaseVerifier(cfg.Workspace.Repo, gitExecutable(), runInspectTimeout)
	if err != nil {
		return nil, err
	}
	runs, err := runapi.New(runapi.Config{
		WorkspaceID: durableWorkspace.ID, RepositoryID: durableWorkspace.RepositoryID,
		RepositoryRemote:            "https://github.com/" + github.Repository.FullName,
		BackgroundImageIdentity:     cfg.Runs.BackgroundImageID,
		BackgroundEnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil),
		Store:                       store, Route: route, Generator: ids, ActorResolver: domain.ContextActor,
		BaseVerifier: baseVerifier, Now: time.Now, RunTimeout: cfg.Runs.RunTimeout, Agent: cfg.Runs.Agent,
		ModelProvider: cfg.Runs.Model.Provider, Model: cfg.Runs.Model.ID, RetentionVerifier: resultSource,
		SealPolicyVersion: "fern.background-user-seal.v1", Wake: coordinator.Wake,
	})
	if err != nil {
		return nil, err
	}
	status.Healthy(observability.ComponentBackgroundRunProfile)
	status.Healthy(observability.ComponentBackgroundRunSerial)
	closeArtifact, closeProvider = false, false
	return &runServices{store: store, runs: runs,
		background: coordinator, provider: provider, artifact: artifact, status: status}, nil
}

// stateRoots are the per-workspace durable directories under ~/.fern/runs.
type stateRoots struct {
	provider, cas, work string
}

// prepareStateRoots creates the workspace's Background Run state directories,
// resolving symlinks so every component sees the same canonical paths.
func prepareStateRoots(workspace string) (stateRoots, error) {
	runDirectory, err := statePath("runs")
	if err != nil {
		return stateRoots{}, err
	}
	backgroundRoot := filepath.Join(runDirectory, workspace+"-background")
	if err := os.MkdirAll(backgroundRoot, 0o700); err != nil {
		return stateRoots{}, fmt.Errorf("create background run state root: %w", err)
	}
	backgroundRoot, err = filepath.EvalSymlinks(backgroundRoot)
	if err != nil {
		return stateRoots{}, err
	}
	roots := stateRoots{provider: filepath.Join(backgroundRoot, "runtime"),
		cas: filepath.Join(backgroundRoot, "artifact-cas"), work: filepath.Join(backgroundRoot, "artifact-work")}
	for _, root := range []string{roots.provider, roots.cas, roots.work} {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return stateRoots{}, fmt.Errorf("create background state root: %w", err)
		}
	}
	return roots, nil
}

// inspectRetainedArtifacts re-verifies every artifact the store references
// before the run profile may be declared qualified.
func inspectRetainedArtifacts(ctx context.Context, store *taskstore.Store, artifact *taskartifact.Engine) error {
	referencedArtifacts, err := store.ReferencedArtifactManifestSHA256(ctx)
	if err != nil {
		return fmt.Errorf("list retained artifacts: %w", err)
	}
	for _, digest := range referencedArtifacts {
		locator, err := taskartifact.ParseLocator("sha256:" + hex.EncodeToString(digest[:]))
		if err != nil {
			return err
		}
		if _, err := artifact.Inspect(ctx, locator); err != nil {
			return fmt.Errorf("reconcile retained artifact: %w", err)
		}
	}
	return nil
}

// ensureWorkspace records the configured repository binding, keeping the
// durable identity of an existing workspace while EnsureWorkspace still checks
// every repository and GitHub authority field against the configuration.
func ensureWorkspace(ctx context.Context, cfg config.Config, store *taskstore.Store, ids *domain.Generator) (taskstore.Workspace, error) {
	candidateID, err := ids.WorkspaceID()
	if err != nil {
		return taskstore.Workspace{}, err
	}
	github := cfg.Workspace.GitHub
	desired := taskstore.Workspace{ID: candidateID, Name: cfg.Workspace.Name, State: taskstore.WorkspaceActive,
		RepositoryPath: cfg.Workspace.Repo, GitHubAuthority: taskstore.GitHubAuthorityAppBroker,
		InstallationID: domain.InstallationID(github.InstallationID), RepositoryID: domain.RepositoryID(github.Repository.ID),
		RepositoryFullName: github.Repository.FullName, ImageDigest: cfg.Runs.BackgroundImageID,
		OpenCodeProtocol: runapi.APIContractVersion, RuntimeDesiredState: "disposable", ReconciliationEpoch: 1,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if existing, err := store.GetWorkspaceByName(ctx, cfg.Workspace.Name); err == nil {
		desired.ID, desired.ImageDigest, desired.OpenCodeProtocol = existing.ID, existing.ImageDigest, existing.OpenCodeProtocol
		desired.RuntimeDesiredState, desired.ReconciliationEpoch, desired.CreatedAt = existing.RuntimeDesiredState, existing.ReconciliationEpoch, existing.CreatedAt
	} else if !errors.Is(err, taskstore.ErrNotFound) {
		return taskstore.Workspace{}, err
	}
	return store.EnsureWorkspace(ctx, desired)
}

// resolveGitHubAuthority loads the stored App credentials and returns the
// installation token source for the configured repository binding.
func resolveGitHubAuthority(github config.GitHubApp) (githubapp.InstallationTokenSource, error) {
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
	if _, err := githubapp.NewRepositoryIdentity(int64(github.InstallationID), int64(github.Repository.ID)); err != nil {
		return nil, err
	}
	return tokens, nil
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
