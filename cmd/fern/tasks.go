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
	"github.com/nebler/fern/internal/runapi"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskapi"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskdelivery"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskexecution"
	"github.com/nebler/fern/internal/taskpublication"
	"github.com/nebler/fern/internal/taskpublicationcoord"
	"github.com/nebler/fern/internal/taskresult"
	"github.com/nebler/fern/internal/taskresultcoord"
	"github.com/nebler/fern/internal/taskresultsource"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/taskverification"
	"github.com/nebler/fern/internal/verification"
	"github.com/nebler/fern/internal/workspace"
	"github.com/nebler/fern/internal/workspacegithub"
)

const (
	taskAPIContractVersion         = "fern.task.v1"
	taskExecutionContractVersion   = "fern.execution.v1"
	taskOpenCodeProtocol           = "0.0.0-next-17444"
	publicationBrokerPolicyVersion = "fern.github-app-publication.v1"
	publicationBrokerPolicy        = "immutable changed result; current uncanceled owner; exact successful verification; derived repository, base, commit, and branch; draft pull request"
	taskSessionDirectory           = "/home/user/workspace"

	// taskServiceCredentialID identifies Fern's internal coordinators in task
	// audit trails; every system actor authenticates with it.
	taskServiceCredentialID = "service-v1"

	// taskPollInterval keeps coordinator loops responsive to wake signals
	// without hot-spinning the shared task store.
	taskPollInterval = time.Second

	// taskOperationTimeout bounds one durable operation attempt: long enough
	// for a slow Git or GitHub round trip, short enough to retry meaningfully.
	taskOperationTimeout = 2 * time.Minute

	// taskInspectTimeout bounds local image inspection and runtime GitHub reads.
	taskInspectTimeout = 15 * time.Second
)

type taskServices struct {
	store        *taskstore.Store
	handler      http.Handler
	runs         http.Handler
	coordinator  taskDeliveryService
	execution    taskRunService
	verification taskRunService
	publication  taskPublicationService
	result       taskResultCoordinator
	resultWake   chan struct{}
	status       *observability.Registry
	background   taskDeliveryService
	provider     *taskenvdocker.Provider
	artifact     *taskartifact.Engine
}

func (services *taskServices) Close() error {
	var providerErr error
	var artifactErr error
	if services.artifact != nil {
		artifactErr = services.artifact.Close()
	}
	if services.provider != nil {
		providerErr = services.provider.Close()
	}
	return errors.Join(artifactErr, providerErr, services.store.Close())
}

type taskRunService interface {
	Run(context.Context) error
}

type taskDeliveryService interface {
	taskRunService
	Wake()
}

type taskPublicationService interface {
	taskRunService
	Wake()
}

type taskResultCoordinator interface {
	RunOnce(context.Context) error
}

func newTaskServices(ctx context.Context, cfg config.Config, docker *runtime.Docker, manager *workspace.Manager, route *backgroundroute.Manager, auth runtime.ServerAuth, status *observability.Registry, log *slog.Logger) (*taskServices, error) {
	if cfg.Tasks == nil {
		return nil, nil
	}
	github := cfg.Workspace.GitHub
	if github == nil {
		return nil, errors.New("task service requires an explicit GitHub authority")
	}
	inspectCtx, inspectCancel := context.WithTimeout(ctx, taskInspectTimeout)
	imageID, err := docker.ResolveImageID(inspectCtx, cfg.Workspace.Image)
	inspectCancel()
	if err != nil {
		return nil, err
	}
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
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = store.Close()
		}
	}()

	ids := task.NewSecureGenerator()
	authority, err := resolveGitHubAuthority(github, manager, docker, cfg.Workspace.Name)
	if err != nil {
		if errors.Is(err, githubapp.ErrCredentialsNotFound) {
			status.Blocked(observability.ComponentGitHubTaskDependency, err)
		}
		return nil, err
	}
	status.Healthy(observability.ComponentGitHubTaskDependency)
	candidateID, err := ids.WorkspaceID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	durableWorkspace, err := store.EnsureWorkspace(ctx, taskstore.Workspace{
		ID: candidateID, Name: cfg.Workspace.Name, State: taskstore.WorkspaceActive,
		RepositoryPath: cfg.Workspace.Repo, GitHubAuthority: authority.kind, InstallationID: task.InstallationID(github.InstallationID),
		RepositoryID: task.RepositoryID(github.Repository.ID), RepositoryFullName: github.Repository.FullName,
		ImageDigest: imageID, OpenCodeProtocol: taskOpenCodeProtocol, RuntimeDesiredState: "running",
		ReconciliationEpoch: 1, CreatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	clientFactory, err := taskdelivery.LocalClientFactory(auth)
	if err != nil {
		return nil, err
	}
	workerID, err := taskWorkerID()
	if err != nil {
		return nil, err
	}
	var artifactEngine *taskartifact.Engine
	var backgroundProvider *taskenvdocker.Provider
	defer func() {
		if closeOnError {
			if backgroundProvider != nil {
				_ = backgroundProvider.Close()
			}
			if artifactEngine != nil {
				_ = artifactEngine.Close()
			}
		}
	}()
	backgroundRoot := filepath.Join(taskDirectory, cfg.Workspace.Name+"-background")
	if err := os.MkdirAll(backgroundRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create background run state root: %w", err)
	}
	backgroundRoot, err = filepath.EvalSymlinks(backgroundRoot)
	if err != nil {
		return nil, err
	}
	providerRoot := filepath.Join(backgroundRoot, "runtime")
	casRoot, workRoot := filepath.Join(backgroundRoot, "artifact-cas"), filepath.Join(backgroundRoot, "artifact-work")
	for _, root := range []string{providerRoot, casRoot, workRoot} {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create background artifact state root: %w", err)
		}
	}
	artifactEngine, err = taskartifact.New(taskartifact.Config{GitExecutable: verificationGitExecutable(), CASRoot: casRoot,
		WorkRoot: workRoot, CommandTimeout: taskOperationTimeout})
	if err != nil {
		return nil, err
	}
	referenced, err := store.ReferencedArtifactManifestSHA256(ctx)
	if err != nil {
		return nil, err
	}
	for _, digest := range referenced {
		locator, parseErr := taskartifact.ParseLocator("sha256:" + hex.EncodeToString(digest[:]))
		if parseErr != nil {
			return nil, parseErr
		}
		if _, inspectErr := artifactEngine.Inspect(ctx, locator); inspectErr != nil {
			return nil, fmt.Errorf("reconcile retained artifact: %w", inspectErr)
		}
	}
	resultSource, err := taskresultsource.New(store, artifactEngine, cfg.Workspace.Repo)
	if err != nil {
		return nil, err
	}
	deliveryActor := systemActor(workerID, "task-delivery", "Task delivery coordinator")
	recoveryActor := systemActor(workerID, "task-recovery", "Task recovery coordinator")
	recoveryActor.Type = task.ActorRecovery
	resultCoordinator, sealAuthorizer, err := newResultSealing(store, manager, cfg, durableWorkspace.ID, workerID)
	if err != nil {
		return nil, err
	}
	resultWake := make(chan struct{}, 1)
	// Prime the first seal sweep so results recorded before this coordinator
	// started are sealed without waiting for the periodic ticker.
	resultWake <- struct{}{}
	coordinator, err := taskdelivery.New(store, manager, clientFactory, ids, taskdelivery.Config{
		WorkspaceID: durableWorkspace.ID, WorkerID: workerID, SessionDirectory: taskSessionDirectory,
		LeaseDuration: cfg.Tasks.LeaseDuration, OperationTimeout: min(cfg.Tasks.LeaseDuration/2, 30*time.Second),
		PollInterval: taskPollInterval, Actor: deliveryActor, RecoveryActor: recoveryActor, Now: time.Now,
		OnError: func(err error) {
			status.Degraded(observability.ComponentTaskDelivery, err)
			log.Error("task coordination deferred", "err", err, "workspace", cfg.Workspace.Name)
		},
		OnSuccess: func() { status.Healthy(observability.ComponentTaskDelivery) },
	})
	if err != nil {
		return nil, err
	}
	executionClients, err := taskexecution.LocalClientFactory(auth)
	if err != nil {
		return nil, err
	}
	executionCoordinator, err := taskexecution.New(store, manager, executionClients, ids, taskexecution.Config{
		WorkspaceID: durableWorkspace.ID, SessionDirectory: taskSessionDirectory,
		APIContractVersion: taskAPIContractVersion, OperationTimeout: 30 * time.Second,
		PollInterval: taskPollInterval, Actor: systemActor(workerID, "task-execution", "Task execution observer"),
		RecoveryActor: recoveryActor, Now: time.Now,
		OnError: func(err error) {
			status.Degraded(observability.ComponentTaskExecution, err)
			log.Error("task execution observation deferred", "err", err, "workspace", cfg.Workspace.Name)
		},
		OnSuccess: func() { status.Healthy(observability.ComponentTaskExecution) },
	})
	if err != nil {
		return nil, err
	}
	publicationCoordinator, err := newPublicationCoordinator(ctx, taskDirectory, cfg, durableWorkspace.ID, workerID, authority, recoveryActor, store, manager, resultSource, ids, status, log)
	if err != nil {
		return nil, err
	}
	var publicationWake func()
	if publicationCoordinator != nil {
		publicationWake = publicationCoordinator.Wake
	}
	verificationCoordinator, err := newVerificationCoordinator(store, manager, resultSource, ids, cfg, durableWorkspace.ID, imageID, workerID, recoveryActor, status, log)
	if err != nil {
		return nil, err
	}
	budget, err := json.Marshal(struct {
		MaxTurns int `json:"maxTurns"`
	}{cfg.Tasks.Budget.MaxTurns})
	if err != nil {
		return nil, err
	}
	handler, err := taskapi.New(taskapi.Config{
		WorkspaceID: durableWorkspace.ID, RepositoryID: durableWorkspace.RepositoryID,
		Store: store, Generator: ids, ActorResolver: taskapi.ContextActor, BaseResolver: authority.baseResolver,
		SealAuthorizer: sealAuthorizer, Wake: coordinator.Wake, SealWake: func() {
			select {
			case resultWake <- struct{}{}:
			default:
			}
		}, PublicationWake: publicationWake, PublicationPolicyVersion: publicationBrokerPolicyVersion,
		PublicationPolicySHA256: sha256.Sum256([]byte(publicationBrokerPolicy)),
		Now:                     time.Now, AttemptTimeout: cfg.Tasks.AttemptTimeout,
		ObjectFormat: "sha1", APIContractVersion: taskAPIContractVersion,
		ExecutionContractVersion: taskExecutionContractVersion, Agent: cfg.Tasks.Agent,
		ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID, BudgetSnapshot: budget,
	})
	if err != nil {
		return nil, err
	}
	baseVerifier, err := runapi.NewGitBaseVerifier(cfg.Workspace.Repo, verificationGitExecutable(), taskInspectTimeout)
	if err != nil {
		return nil, err
	}
	hostname := github.Hostname
	if hostname == "" {
		hostname = "github.com"
	}
	availableProfile, backgroundImageID := "", ""
	var backgroundCoordinator *backgroundruncoord.Coordinator
	if cfg.Tasks.BackgroundImage != "" {
		backgroundInspectCtx, backgroundInspectCancel := context.WithTimeout(ctx, taskInspectTimeout)
		backgroundImageID, err = docker.ResolveBackgroundRunImageID(backgroundInspectCtx, cfg.Tasks.BackgroundImage, cfg.Tasks.BackgroundImageID)
		backgroundInspectCancel()
		if err != nil {
			status.Blocked(observability.ComponentBackgroundRunProfile, err)
			return nil, err
		}
		availableProfile = runapi.PluginOpenCodeProfile
		status.Qualified(observability.ComponentBackgroundRunProfile)
		backgroundRepository, err := filepath.EvalSymlinks(cfg.Workspace.Repo)
		if err != nil {
			return nil, fmt.Errorf("resolve background run repository: %w", err)
		}
		// Serial profile policy: 1 GiB RAM, 2 CPU, 512 PIDs, 10 GiB source/clone
		// admission, 20 GiB host free admission, 10 MiB x 3 logs, and 24h wall.
		backgroundProvider, err = taskenvdocker.New(ctx, taskenvdocker.Config{
			StateRoot: providerRoot, Repository: backgroundRepository, GitExecutable: verificationGitExecutable(),
			ImageReference: cfg.Tasks.BackgroundImage, ImageID: backgroundImageID, MemoryBytes: 1 << 30,
			NanoCPUs: 2_000_000_000, PIDs: 512, WallTimeout: 24 * time.Hour, GitTimeout: 2 * time.Minute,
			DockerTimeout: 30 * time.Second, HealthTimeout: 30 * time.Second, GitOutputBytes: 1 << 20,
			SourceSizeAdmissionBytes: 10 << 30, CloneObservedLimitBytes: 10 << 30, DiskFreeAdmissionBytes: 20 << 30,
			LogMaxSize: "10m", LogMaxFiles: 3, StopGrace: 10 * time.Second, Environment: backgroundRunEnvironment(cfg),
		}, docker.BackgroundRunDockerClient())
		if err != nil {
			status.Blocked(observability.ComponentBackgroundRunSerial, err)
			return nil, err
		}
		backgroundCoordinator, err = backgroundruncoord.New(store, backgroundProvider, artifactEngine, ids, backgroundruncoord.Config{
			WorkspaceID: durableWorkspace.ID, WorkerID: workerID, SystemActor: systemActor(workerID, "background-run", "Background Run coordinator"),
			Profile: availableProfile, ImageIdentity: backgroundImageID, EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(backgroundRunEnvironment(cfg)), Agent: cfg.Tasks.Agent,
			ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID,
			OperationTimeout: min(cfg.Tasks.LeaseDuration/2, 30*time.Second), LeaseDuration: cfg.Tasks.LeaseDuration,
			PollInterval: taskPollInterval, HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 100, MaxPages: 100, MaxEvents: 10000},
			Now: time.Now, HTTPClient: &http.Client{Timeout: min(cfg.Tasks.LeaseDuration/2, 30*time.Second)}, Route: route,
			OnError: func(err error) {
				status.Degraded(observability.ComponentBackgroundRunSerial, err)
				log.Error("background run coordination deferred", "err", err, "workspace", cfg.Workspace.Name)
			},
			OnSuccess: func() { status.Healthy(observability.ComponentBackgroundRunSerial) },
		})
		if err != nil {
			return nil, err
		}
		status.Healthy(observability.ComponentBackgroundRunSerial)
	}
	var runRoute runapi.RouteResolver
	if availableProfile != "" {
		runRoute = route
	}
	runs, err := runapi.New(runapi.Config{
		WorkspaceID: durableWorkspace.ID, RepositoryID: durableWorkspace.RepositoryID,
		RepositoryRemote:        "https://" + hostname + "/" + github.Repository.FullName,
		BackgroundImageIdentity: backgroundImageID, BackgroundEnvironmentSHA256: taskenvdocker.EnvironmentSHA256(backgroundRunEnvironment(cfg)), AvailableProfile: availableProfile,
		Store: store, Generator: ids, ActorResolver: taskapi.ContextActor, BaseVerifier: baseVerifier,
		Now: time.Now, AttemptTimeout: cfg.Tasks.AttemptTimeout, Agent: cfg.Tasks.Agent,
		ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID, BudgetSnapshot: budget,
		Route:             runRoute,
		SealPolicyVersion: "fern.background-user-seal.v1",
		Wake: func() {
			if backgroundCoordinator != nil {
				backgroundCoordinator.Wake()
			}
		},
	})
	if err != nil {
		return nil, err
	}
	closeOnError = false
	status.Healthy(observability.ComponentTaskDelivery)
	status.Healthy(observability.ComponentTaskExecution)
	status.Healthy(observability.ComponentTaskResult)
	var publicationService taskPublicationService
	if publicationCoordinator != nil {
		publicationService = publicationCoordinator
	}
	var verificationService taskRunService
	if verificationCoordinator != nil {
		verificationService = verificationCoordinator
	}
	var backgroundService taskDeliveryService
	if backgroundCoordinator != nil {
		backgroundService = backgroundCoordinator
	}
	return &taskServices{
		store: store, handler: handler, runs: runs, coordinator: coordinator,
		execution: executionCoordinator, verification: verificationService, publication: publicationService,
		result: resultCoordinator, resultWake: resultWake, status: status,
		background: backgroundService, provider: backgroundProvider, artifact: artifactEngine,
	}, nil
}

func backgroundRunEnvironment(cfg config.Config) map[string]string {
	if cfg.Tasks == nil {
		return nil
	}
	return maps.Clone(cfg.Tasks.BackgroundEnvironment)
}

// gitHubAuthority bundles how tasks authenticate to GitHub and resolve base refs.
type gitHubAuthority struct {
	kind               taskstore.GitHubAuthority
	baseResolver       taskapi.BaseResolver
	installationTokens githubapp.InstallationTokenSource
	repositories       *githubapp.RepositoryClient
}

// resolveGitHubAuthority validates local authority material during composition.
// Exact remote repository identity remains validated by task admission and
// publication operations, where transient GitHub failures can be retried.
func resolveGitHubAuthority(github *config.WorkspaceGitHub, manager *workspace.Manager, docker *runtime.Docker, workspaceName string) (*gitHubAuthority, error) {
	switch github.Mode {
	case config.GitHubModeGitHubAppBroker:
		credentialsDirectory, err := statePath("github-app")
		if err != nil {
			return nil, err
		}
		credentialStore, err := githubapp.NewCredentialStore(credentialsDirectory)
		if err != nil {
			return nil, err
		}
		credentials, err := credentialStore.Load()
		if err != nil {
			return nil, fmt.Errorf("load GitHub App credentials for task service: %w", err)
		}
		signer, err := githubapp.NewJWTSigner(credentials.AppID(), credentials.PrivateKey())
		if err != nil {
			return nil, err
		}
		installationTokens, err := githubapp.NewClient(http.DefaultClient, signer)
		if err != nil {
			return nil, err
		}
		repositories, err := githubapp.NewRepositoryClient(http.DefaultClient, installationTokens, time.Now)
		if err != nil {
			return nil, err
		}
		identity, err := githubapp.NewRepositoryIdentity(github.InstallationID, github.Repository.ID)
		if err != nil {
			return nil, err
		}
		baseResolver, err := taskapi.GitHubBaseResolver(repositories, identity, github.Repository.FullName, taskInspectTimeout)
		if err != nil {
			return nil, err
		}
		return &gitHubAuthority{
			kind: taskstore.GitHubAuthorityAppBroker, baseResolver: baseResolver,
			installationTokens: installationTokens, repositories: repositories,
		}, nil
	case config.GitHubModeWorkspaceGH:
		executor, err := workspacegithub.NewManagedExecutor(manager, docker, workspaceName, taskInspectTimeout)
		if err != nil {
			return nil, err
		}
		client, err := workspacegithub.New(executor, github.Hostname)
		if err != nil {
			return nil, err
		}
		return &gitHubAuthority{kind: taskstore.GitHubAuthorityWorkspaceGH, baseResolver: func(resolveContext context.Context, ref string) (task.GitOID, error) {
			branch, err := client.Branch(resolveContext, github.Repository.ID, github.Repository.FullName, ref)
			if err != nil {
				return "", err
			}
			return task.ParseGitOID(branch.SHA)
		}}, nil
	default:
		return nil, errors.New("task service requires a supported GitHub authority")
	}
}

// systemActor builds the internal actor snapshot attributed to coordinator
// actions; the request ID carries the worker identity for claim attribution.
func systemActor(workerID, id, displayName string) task.ActorSnapshot {
	return task.ActorSnapshot{
		Type: task.ActorSystem, ID: id, DisplayName: displayName,
		CredentialID: taskServiceCredentialID, Authentication: "internal", RequestID: workerID,
	}
}

// newResultSealing builds the user-authorized result collector, its sealing
// coordinator, and the API-facing seal authorizer sharing one user-seal policy.
func newResultSealing(store *taskstore.Store, manager *workspace.Manager, cfg config.Config, workspaceID task.WorkspaceID, workerID string) (*taskresultcoord.Coordinator, *taskresultcoord.Authorizer, error) {
	resultCollector, err := taskresult.New(taskresult.Config{
		GitExecutable: verificationGitExecutable(), Timeout: time.Minute, OutputBytes: 64 << 20, Now: time.Now,
	})
	if err != nil {
		return nil, nil, err
	}
	resultCoordinator, err := taskresultcoord.NewAuthorized(store, manager, resultCollector, taskresultcoord.Config{
		WorkspaceID: workspaceID, RepositoryPath: cfg.Workspace.Repo, PolicyVersion: "fern.user-seal.v1",
		OperationTimeout: taskOperationTimeout, Actor: systemActor(workerID, "task-result", "Task result coordinator"),
		ClaimOwner: workerID, Now: time.Now,
	})
	if err != nil {
		return nil, nil, err
	}
	sealAuthorizer, err := taskresultcoord.NewAuthorizer(store, manager, resultCollector, taskresultcoord.AuthorizerConfig{
		RepositoryPath: cfg.Workspace.Repo, PolicyVersion: "fern.user-seal.v1", OperationTimeout: taskOperationTimeout,
	})
	if err != nil {
		return nil, nil, err
	}
	return resultCoordinator, sealAuthorizer, nil
}

// newPublicationCoordinator builds the App-broker reconciler that opens draft
// pull requests from immutable verified results. It returns nil unless the
// workspace delegates GitHub authority to the App broker.
func newPublicationCoordinator(parent context.Context, taskDirectory string, cfg config.Config, workspaceID task.WorkspaceID, workerID string, authority *gitHubAuthority, recoveryActor task.ActorSnapshot, store *taskstore.Store, manager *workspace.Manager, source taskpublicationcoord.ResultSource, ids *task.Generator, status *observability.Registry, log *slog.Logger) (*taskpublicationcoord.Coordinator, error) {
	if cfg.Workspace.GitHub.Mode != config.GitHubModeGitHubAppBroker {
		return nil, nil
	}
	publicationTemp := filepath.Join(taskDirectory, cfg.Workspace.Name+"-publication")
	if err := os.MkdirAll(publicationTemp, 0o700); err != nil {
		return nil, fmt.Errorf("create task publication temporary directory: %w", err)
	}
	publisher, err := taskpublication.New(taskpublication.Config{
		RepositoryPath: cfg.Workspace.Repo, GitExecutable: "/usr/bin/git", TempRoot: publicationTemp,
		Timeout: taskOperationTimeout, OutputLimit: 64 << 10, Now: time.Now,
	}, authority.installationTokens, authority.repositories)
	if err != nil {
		return nil, err
	}
	coordinator, err := taskpublicationcoord.New(store, manager, publisher, ids, taskpublicationcoord.Config{
		WorkspaceID:      workspaceID,
		PullRequestBody:  "Created by Fern from an immutable result after the configured verification policy passed.",
		OperationTimeout: taskOperationTimeout, PollInterval: taskPollInterval,
		Actor:         systemActor(workerID, "task-publication", "Task publication coordinator"),
		RecoveryActor: recoveryActor, Now: time.Now,
		ResultSource: source,
		OnError: func(err error) {
			status.Degraded(observability.ComponentTaskPublication, err)
			log.Error("task publication reconciliation deferred", "err", err, "workspace", cfg.Workspace.Name)
		},
		OnSuccess: func() { status.Healthy(observability.ComponentTaskPublication) },
	})
	if err != nil {
		return nil, err
	}
	status.Healthy(observability.ComponentTaskPublication)
	return coordinator, nil
}

// newVerificationCoordinator builds the host-side verification coordinator when
// a verification policy is configured; it returns nil otherwise.
func newVerificationCoordinator(store *taskstore.Store, manager *workspace.Manager, source taskverification.ResultSource, ids *task.Generator, cfg config.Config, workspaceID task.WorkspaceID, imageID, workerID string, recoveryActor task.ActorSnapshot, status *observability.Registry, log *slog.Logger) (*taskverification.Coordinator, error) {
	configured := cfg.Tasks.Verification
	if configured == nil {
		return nil, nil
	}
	policy, err := verification.NewPolicy(verification.PolicyConfig{
		CheckName: configured.CheckName, Argv: configured.Argv, WorkingDirectory: configured.WorkingDirectory,
		Timeout: configured.Timeout, Environment: configured.Environment, OutputBytes: configured.OutputBytes,
	})
	if err != nil {
		return nil, err
	}
	runner, err := verification.NewRunner(verification.RunnerConfig{
		GitExecutable: verificationGitExecutable(), GitTimeout: 30 * time.Second,
		Environment: map[string]string{
			"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
			"HOME": "/", "LANG": "C", "LC_ALL": "C", "PATH": "/usr/bin:/bin",
		},
		Name: "fern-host", Version: version + "@" + commit, ImageDigest: imageID,
	})
	if err != nil {
		return nil, err
	}
	coordinator, err := taskverification.New(store, manager, runner, policy, ids, taskverification.Config{
		WorkspaceID: workspaceID, RepositoryPath: cfg.Workspace.Repo,
		PollInterval: taskPollInterval, Deadline: configured.Timeout + taskOperationTimeout,
		Actor:         systemActor(workerID, "task-verification", "Task verification coordinator"),
		RecoveryActor: recoveryActor, Now: time.Now,
		ResultSource: source,
		OnError: func(err error) {
			status.Degraded(observability.ComponentTaskVerification, err)
			log.Error("task verification deferred", "err", err, "workspace", cfg.Workspace.Name)
		},
		OnSuccess: func() { status.Healthy(observability.ComponentTaskVerification) },
	})
	if err != nil {
		return nil, err
	}
	status.Healthy(observability.ComponentTaskVerification)
	return coordinator, nil
}

func runTaskResultCoordinator(ctx context.Context, service *taskServices, log *slog.Logger, workspaceName string) error {
	retry := observability.NewRetry(taskPollInterval, 30*time.Second)
	var delay time.Duration
	for {
		if err := observability.Wait(ctx, service.resultWake, delay); err != nil {
			return err
		}
		failed := false
		for {
			err := service.result.RunOnce(ctx)
			if err == nil {
				continue
			}
			if errors.Is(err, taskresultcoord.ErrNoWork) {
				break
			}
			if errors.Is(err, context.Canceled) {
				return err
			}
			if errors.Is(err, taskstore.ErrCorruptStore) {
				return err
			}
			log.Error("user-authorized result sealing deferred", "err", err, "workspace", workspaceName)
			failed = true
			break
		}
		if failed {
			service.status.Degraded(observability.ComponentTaskResult, errors.New("result sealing deferred"))
			delay = retry.Next()
		} else {
			service.status.Healthy(observability.ComponentTaskResult)
			retry.Reset()
			delay = taskPollInterval
		}
	}
}

func verificationGitExecutable() string {
	if goruntime.GOOS == "darwin" {
		for _, candidate := range []string{
			"/Library/Developer/CommandLineTools/usr/bin/git",
			"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
		} {
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

func newGitHubOnboarding(cfg config.Config) (http.Handler, error) {
	if cfg.RemoteOrigin == "" || cfg.Workspace.GitHub == nil || cfg.Workspace.GitHub.Mode != config.GitHubModeGitHubAppBroker {
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
	return githubapp.NewOnboardingHTTPWithSetupOrigin(
		cfg.RemoteOrigin, "http://"+cfg.OperatorListen, "Fern "+cfg.Workspace.Name,
		states, exchanger, credentials, rand.Reader, time.Now,
	)
}
