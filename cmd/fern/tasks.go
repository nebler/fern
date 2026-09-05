package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/nebler/fern/internal/resultapi"
	"github.com/nebler/fern/internal/runapi"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskpublication"
	"github.com/nebler/fern/internal/taskpublicationcoord"
	"github.com/nebler/fern/internal/taskresultsource"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/taskverification"
	"github.com/nebler/fern/internal/verification"
)

const (
	publicationBrokerPolicyVersion = "fern.github-app-publication.v1"
	publicationBrokerPolicy        = "immutable retained changed result; current uncanceled owner; exact successful verification; derived repository, base, commit, and branch; draft pull request"
	taskServiceCredentialID        = "service-v1"
	taskPollInterval               = time.Second
	taskOperationTimeout           = 2 * time.Minute
	taskInspectTimeout             = 15 * time.Second
	backgroundCloneTimeout         = 30 * time.Second
	backgroundCloneAdmissionBytes  = 128 << 20
)

type taskRunService interface {
	Run(context.Context) error
}

type taskWakeService interface {
	taskRunService
	Wake()
}

type taskServices struct {
	store        *taskstore.Store
	runs         http.Handler
	results      http.Handler
	verification taskRunService
	publication  taskRunService
	background   taskWakeService
	provider     *taskenvdocker.Provider
	artifact     *taskartifact.Engine
	status       *observability.Registry
}

func (services *taskServices) Close() error {
	return errors.Join(services.artifact.Close(), services.provider.Close(), services.store.Close())
}

func newTaskServices(ctx context.Context, cfg config.BackgroundConfig, route *backgroundroute.Manager, status *observability.Registry, log *slog.Logger) (*taskServices, error) {
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
	workerID, err := taskWorkerID()
	if err != nil {
		return nil, err
	}
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
	artifact, err := taskartifact.New(taskartifact.Config{GitExecutable: verificationGitExecutable(), CASRoot: casRoot,
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

	repository, err := filepath.EvalSymlinks(cfg.Workspace.Repo)
	if err != nil {
		return nil, fmt.Errorf("resolve Background Run repository: %w", err)
	}
	provider, err := taskenvdocker.New(ctx, taskenvdocker.Config{
		StateRoot: providerRoot, Repository: repository, GitExecutable: verificationGitExecutable(),
		ImageReference: cfg.Tasks.BackgroundImage, ImageID: cfg.Tasks.BackgroundImageID, MemoryBytes: 1 << 30,
		NanoCPUs: 2_000_000_000, PIDs: 512, WallTimeout: 24 * time.Hour, GitTimeout: backgroundCloneTimeout,
		DockerTimeout: 30 * time.Second, HealthTimeout: 30 * time.Second, GitOutputBytes: 1 << 20,
		SourceSizeAdmissionBytes: backgroundCloneAdmissionBytes, CloneObservedLimitBytes: backgroundCloneAdmissionBytes, DiskFreeAdmissionBytes: 20 << 30,
		LogMaxSize: "10m", LogMaxFiles: 3, StopGrace: 10 * time.Second, Environment: backgroundRunEnvironment(cfg),
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
		// Preserve retired runtime metadata in schema-10 databases while still
		// checking every repository and GitHub authority field through EnsureWorkspace.
		desired.ID, desired.ImageDigest, desired.OpenCodeProtocol = existing.ID, existing.ImageDigest, existing.OpenCodeProtocol
		desired.RuntimeDesiredState, desired.ReconciliationEpoch, desired.CreatedAt = existing.RuntimeDesiredState, existing.ReconciliationEpoch, existing.CreatedAt
	} else if !errors.Is(readErr, taskstore.ErrNotFound) {
		return nil, readErr
	}
	durableWorkspace, err := store.EnsureWorkspace(ctx, desired)
	if err != nil {
		return nil, err
	}
	resultSource, err := taskresultsource.New(store, artifact)
	if err != nil {
		return nil, err
	}

	recoveryActor := systemActor(workerID, "task-recovery", "Task recovery coordinator")
	recoveryActor.Type = task.ActorRecovery
	publication, err := newPublicationCoordinator(taskDirectory, cfg, durableWorkspace.ID, workerID, authority, recoveryActor, store, resultSource, ids, status, log)
	if err != nil {
		return nil, err
	}
	verificationCoordinator, err := newVerificationCoordinator(store, resultSource, ids, cfg, durableWorkspace.ID, cfg.Tasks.BackgroundImageID, workerID, recoveryActor, status, log)
	if err != nil {
		return nil, err
	}
	budget, err := json.Marshal(struct {
		MaxTurns int `json:"maxTurns"`
	}{cfg.Tasks.Budget.MaxTurns})
	if err != nil {
		return nil, err
	}
	coordinator, err := backgroundruncoord.New(store, provider, artifact, ids, backgroundruncoord.Config{
		WorkspaceID: durableWorkspace.ID, WorkerID: workerID, SystemActor: systemActor(workerID, "background-run", "Background Run coordinator"),
		Profile: runapi.PluginOpenCodeProfile, ImageIdentity: cfg.Tasks.BackgroundImageID,
		EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(backgroundRunEnvironment(cfg)), Agent: cfg.Tasks.Agent,
		ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID,
		OperationTimeout: min(cfg.Tasks.LeaseDuration/2, backgroundCloneTimeout), LeaseDuration: cfg.Tasks.LeaseDuration,
		PollInterval: taskPollInterval, HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 100, MaxPages: 100, MaxEvents: 10000},
		Now: time.Now, HTTPClient: &http.Client{Timeout: min(cfg.Tasks.LeaseDuration/2, 30*time.Second)}, Route: route,
		OnError: func(err error) {
			status.Degraded(observability.ComponentBackgroundRunSerial, err)
			log.Error("Background Run coordination deferred", "err", err, "repository", cfg.Workspace.Name)
		},
		OnSuccess: func() { status.Healthy(observability.ComponentBackgroundRunSerial) },
	})
	if err != nil {
		return nil, err
	}
	baseVerifier, err := runapi.NewGitBaseVerifier(cfg.Workspace.Repo, verificationGitExecutable(), taskInspectTimeout)
	if err != nil {
		return nil, err
	}
	runs, err := runapi.New(runapi.Config{
		WorkspaceID: durableWorkspace.ID, RepositoryID: durableWorkspace.RepositoryID,
		RepositoryRemote:            "https://github.com/" + github.Repository.FullName,
		BackgroundImageIdentity:     cfg.Tasks.BackgroundImageID,
		BackgroundEnvironmentSHA256: taskenvdocker.EnvironmentSHA256(backgroundRunEnvironment(cfg)),
		AvailableProfile:            runapi.PluginOpenCodeProfile, Store: store, Generator: ids, ActorResolver: task.ContextActor,
		BaseVerifier: baseVerifier, Now: time.Now, AttemptTimeout: cfg.Tasks.AttemptTimeout, Agent: cfg.Tasks.Agent,
		ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID, BudgetSnapshot: budget, Route: route, RetentionVerifier: resultSource,
		SealPolicyVersion: "fern.background-user-seal.v1", Wake: coordinator.Wake,
	})
	if err != nil {
		return nil, err
	}
	results, err := resultapi.New(resultapi.Config{
		WorkspaceID: durableWorkspace.ID, Store: store, Generator: ids, ActorResolver: task.ContextActor,
		Wake: publication.Wake, Now: time.Now, PublicationPolicyVersion: publicationBrokerPolicyVersion,
		PublicationPolicySHA256: sha256.Sum256([]byte(publicationBrokerPolicy)), APIContractVersion: resultapi.APIContractVersion,
	})
	if err != nil {
		return nil, err
	}
	status.Qualified(observability.ComponentBackgroundRunProfile)
	status.Healthy(observability.ComponentBackgroundRunSerial)
	closeStore, closeArtifact, closeProvider = false, false, false
	return &taskServices{store: store, runs: runs, results: results, verification: verificationCoordinator, publication: publication,
		background: coordinator, provider: provider, artifact: artifact, status: status}, nil
}

func backgroundRunEnvironment(cfg config.BackgroundConfig) map[string]string {
	return maps.Clone(cfg.Tasks.BackgroundEnvironment)
}

type gitHubAuthority struct {
	installationTokens githubapp.InstallationTokenSource
	repositories       *githubapp.RepositoryClient
}

func resolveGitHubAuthority(github config.BackgroundGitHubApp) (*gitHubAuthority, error) {
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
	repositories, err := githubapp.NewRepositoryClient(http.DefaultClient, tokens, time.Now)
	if err != nil {
		return nil, err
	}
	if _, err := githubapp.NewRepositoryIdentity(github.InstallationID, github.Repository.ID); err != nil {
		return nil, err
	}
	return &gitHubAuthority{installationTokens: tokens, repositories: repositories}, nil
}

func systemActor(workerID, id, displayName string) task.ActorSnapshot {
	return task.ActorSnapshot{Type: task.ActorSystem, ID: id, DisplayName: displayName,
		CredentialID: taskServiceCredentialID, Authentication: "internal", RequestID: workerID}
}

func newPublicationCoordinator(taskDirectory string, cfg config.BackgroundConfig, workspaceID task.WorkspaceID, workerID string,
	authority *gitHubAuthority, recoveryActor task.ActorSnapshot, store *taskstore.Store, source taskpublicationcoord.ResultSource,
	ids *task.Generator, status *observability.Registry, log *slog.Logger) (*taskpublicationcoord.Coordinator, error) {
	publicationTemp := filepath.Join(taskDirectory, cfg.Workspace.Name+"-publication")
	if err := os.MkdirAll(publicationTemp, 0o700); err != nil {
		return nil, fmt.Errorf("create publication temporary directory: %w", err)
	}
	publisher, err := taskpublication.New(taskpublication.Config{RepositoryPath: cfg.Workspace.Repo,
		GitExecutable: verificationGitExecutable(), TempRoot: publicationTemp, Timeout: taskOperationTimeout,
		OutputLimit: 64 << 10, Now: time.Now}, authority.installationTokens, authority.repositories)
	if err != nil {
		return nil, err
	}
	coordinator, err := taskpublicationcoord.New(store, publisher, ids, taskpublicationcoord.Config{
		WorkspaceID: workspaceID, PullRequestBody: "Created by Fern from an immutable result after verification passed.",
		OperationTimeout: taskOperationTimeout, PollInterval: taskPollInterval,
		Actor: systemActor(workerID, "task-publication", "Task publication coordinator"), RecoveryActor: recoveryActor,
		Now: time.Now, ResultSource: source,
		OnError: func(err error) {
			status.Degraded(observability.ComponentTaskPublication, err)
			log.Error("publication reconciliation deferred", "err", err, "repository", cfg.Workspace.Name)
		}, OnSuccess: func() { status.Healthy(observability.ComponentTaskPublication) },
	})
	if err == nil {
		status.Healthy(observability.ComponentTaskPublication)
	}
	return coordinator, err
}

func newVerificationCoordinator(store *taskstore.Store, source taskverification.ResultSource, ids *task.Generator,
	cfg config.BackgroundConfig, workspaceID task.WorkspaceID, imageID, workerID string, recoveryActor task.ActorSnapshot,
	status *observability.Registry, log *slog.Logger) (*taskverification.Coordinator, error) {
	configured := cfg.Tasks.Verification
	if configured == nil {
		return nil, nil
	}
	policy, err := verification.NewPolicy(verification.PolicyConfig{CheckName: configured.CheckName, Argv: configured.Argv,
		WorkingDirectory: configured.WorkingDirectory, Timeout: configured.Timeout, Environment: configured.Environment,
		OutputBytes: configured.OutputBytes})
	if err != nil {
		return nil, err
	}
	runner, err := verification.NewRunner(verification.RunnerConfig{GitExecutable: verificationGitExecutable(), GitTimeout: 30 * time.Second,
		Environment: map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
			"HOME": "/", "LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin:/bin"},
		Name: "fern-host", Version: version + "@" + commit, ImageDigest: imageID})
	if err != nil {
		return nil, err
	}
	coordinator, err := taskverification.New(store, runner, policy, ids, taskverification.Config{
		WorkspaceID: workspaceID, PollInterval: taskPollInterval,
		Deadline: configured.Timeout + taskOperationTimeout, Actor: systemActor(workerID, "task-verification", "Task verification coordinator"),
		RecoveryActor: recoveryActor, Now: time.Now, ResultSource: source,
		OnError: func(err error) {
			status.Degraded(observability.ComponentTaskVerification, err)
			log.Error("verification deferred", "err", err, "repository", cfg.Workspace.Name)
		}, OnSuccess: func() { status.Healthy(observability.ComponentTaskVerification) },
	})
	if err == nil {
		status.Healthy(observability.ComponentTaskVerification)
	}
	return coordinator, err
}

func verificationGitExecutable() string {
	if goruntime.GOOS == "darwin" {
		for _, candidate := range []string{"/Library/Developer/CommandLineTools/usr/bin/git", "/Applications/Xcode.app/Contents/Developer/usr/bin/git"} {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return candidate
			}
		}
	}
	return "/usr/bin/git"
}

func taskWorkerID() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create task worker identity: %w", err)
	}
	return "worker-" + hex.EncodeToString(random[:]), nil
}

func newGitHubOnboarding(cfg config.BackgroundConfig) (http.Handler, error) {
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
