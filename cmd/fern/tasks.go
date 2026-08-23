package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskapi"
	"github.com/nebler/fern/internal/taskdelivery"
	"github.com/nebler/fern/internal/taskexecution"
	"github.com/nebler/fern/internal/taskpublication"
	"github.com/nebler/fern/internal/taskpublicationcoord"
	"github.com/nebler/fern/internal/taskresult"
	"github.com/nebler/fern/internal/taskresultcoord"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/taskverification"
	"github.com/nebler/fern/internal/verification"
	"github.com/nebler/fern/internal/workspace"
	"github.com/nebler/fern/internal/workspacegithub"
)

const (
	taskAPIContractVersion       = "fern.task.v1"
	taskExecutionContractVersion = "fern.execution.v1"
	taskOpenCodeProtocol         = "0.0.0-next-17444"
	taskSessionDirectory         = "/home/user/workspace"
)

type taskServices struct {
	store        *taskstore.Store
	handler      http.Handler
	coordinator  *taskdelivery.Coordinator
	execution    *taskexecution.Coordinator
	verification *taskverification.Coordinator
	publication  *taskpublicationcoord.Coordinator
	result       *taskresultcoord.Coordinator
	resultWake   chan struct{}
}

func newTaskServices(ctx context.Context, cfg config.Config, docker *runtime.Docker, manager *workspace.Manager, auth runtime.ServerAuth, log *slog.Logger) (*taskServices, error) {
	if cfg.Tasks == nil {
		return nil, nil
	}
	if cfg.Workspace.GitHub == nil {
		return nil, fmt.Errorf("task service requires an explicit GitHub authority")
	}
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 15*time.Second)
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
	var (
		baseResolver       taskapi.BaseResolver
		installationTokens githubapp.InstallationTokenSource
		repositories       *githubapp.RepositoryClient
		githubAuthority    taskstore.GitHubAuthority
	)
	switch cfg.Workspace.GitHub.Mode {
	case config.GitHubModeGitHubAppBroker:
		githubAuthority = taskstore.GitHubAuthorityAppBroker
		credentialsDirectory, stateErr := statePath("github-app")
		if stateErr != nil {
			return nil, stateErr
		}
		credentialStore, credentialErr := githubapp.NewCredentialStore(credentialsDirectory)
		if credentialErr != nil {
			return nil, credentialErr
		}
		credentials, credentialErr := credentialStore.Load()
		if credentialErr != nil {
			return nil, fmt.Errorf("load GitHub App credentials for task service: %w", credentialErr)
		}
		signer, signerErr := githubapp.NewJWTSigner(credentials.AppID(), credentials.PrivateKey())
		if signerErr != nil {
			return nil, signerErr
		}
		installationTokens, err = githubapp.NewClient(http.DefaultClient, signer)
		if err != nil {
			return nil, err
		}
		repositories, err = githubapp.NewRepositoryClient(http.DefaultClient, installationTokens, time.Now)
		if err != nil {
			return nil, err
		}
		identity, identityErr := githubapp.NewRepositoryIdentity(cfg.Workspace.GitHub.InstallationID, cfg.Workspace.GitHub.Repository.ID)
		if identityErr != nil {
			return nil, identityErr
		}
		authorityContext, authorityCancel := context.WithTimeout(ctx, 15*time.Second)
		_, err = repositories.RepositoryByID(authorityContext, identity, cfg.Workspace.GitHub.Repository.FullName)
		authorityCancel()
		if err != nil {
			return nil, fmt.Errorf("validate configured GitHub App repository authority: %w", err)
		}
		baseResolver, err = taskapi.GitHubBaseResolver(repositories, identity, cfg.Workspace.GitHub.Repository.FullName, 15*time.Second)
		if err != nil {
			return nil, err
		}
	case config.GitHubModeWorkspaceGH:
		githubAuthority = taskstore.GitHubAuthorityWorkspaceGH
		executor, executorErr := workspacegithub.NewManagedExecutor(manager, docker, cfg.Workspace.Name, 15*time.Second)
		if executorErr != nil {
			return nil, executorErr
		}
		client, clientErr := workspacegithub.New(executor, cfg.Workspace.GitHub.Hostname)
		if clientErr != nil {
			return nil, clientErr
		}
		baseResolver = func(resolveContext context.Context, ref string) (task.GitOID, error) {
			branch, resolveErr := client.Branch(resolveContext, cfg.Workspace.GitHub.Repository.ID, cfg.Workspace.GitHub.Repository.FullName, ref)
			if resolveErr != nil {
				return "", resolveErr
			}
			return task.ParseGitOID(branch.SHA)
		}
	default:
		return nil, fmt.Errorf("task service requires a supported GitHub authority")
	}
	candidateID, err := ids.WorkspaceID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	durableWorkspace, err := store.EnsureWorkspace(ctx, taskstore.Workspace{
		ID: candidateID, Name: cfg.Workspace.Name, State: taskstore.WorkspaceActive,
		RepositoryPath: cfg.Workspace.Repo, GitHubAuthority: githubAuthority, InstallationID: task.InstallationID(cfg.Workspace.GitHub.InstallationID),
		RepositoryID: task.RepositoryID(cfg.Workspace.GitHub.Repository.ID), RepositoryFullName: cfg.Workspace.GitHub.Repository.FullName,
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
	systemActor := task.ActorSnapshot{
		Type: task.ActorSystem, ID: "task-delivery", DisplayName: "Task delivery coordinator",
		CredentialID: "service-v1", Authentication: "internal", RequestID: workerID,
	}
	recoveryActor := task.ActorSnapshot{
		Type: task.ActorRecovery, ID: "task-recovery", DisplayName: "Task recovery coordinator",
		CredentialID: "service-v1", Authentication: "internal", RequestID: workerID,
	}
	resultCollector, err := taskresult.New(taskresult.Config{
		GitExecutable: verificationGitExecutable(), Timeout: time.Minute, OutputBytes: 64 << 20, Now: time.Now,
	})
	if err != nil {
		return nil, err
	}
	resultActor := task.ActorSnapshot{
		Type: task.ActorSystem, ID: "task-result", DisplayName: "Task result coordinator",
		CredentialID: "service-v1", Authentication: "internal", RequestID: workerID,
	}
	resultCoordinator, err := taskresultcoord.NewAuthorized(store, manager, resultCollector, taskresultcoord.Config{
		WorkspaceID: durableWorkspace.ID, RepositoryPath: cfg.Workspace.Repo, PolicyVersion: "fern.user-seal.v1",
		OperationTimeout: 2 * time.Minute, Actor: resultActor, ClaimOwner: workerID, Now: time.Now,
	})
	if err != nil {
		return nil, err
	}
	sealAuthorizer, err := taskresultcoord.NewAuthorizer(store, manager, resultCollector, taskresultcoord.AuthorizerConfig{
		RepositoryPath: cfg.Workspace.Repo, PolicyVersion: "fern.user-seal.v1", OperationTimeout: 2 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	resultWake := make(chan struct{}, 1)
	resultWake <- struct{}{}
	coordinator, err := taskdelivery.New(store, manager, clientFactory, ids, taskdelivery.Config{
		WorkspaceID: durableWorkspace.ID, WorkerID: workerID, SessionDirectory: taskSessionDirectory,
		LeaseDuration: cfg.Tasks.LeaseDuration, OperationTimeout: min(cfg.Tasks.LeaseDuration/2, 30*time.Second),
		PollInterval: time.Second, Actor: systemActor, RecoveryActor: recoveryActor, Now: time.Now,
		OnError: func(err error) { log.Error("task coordination deferred", "err", err, "workspace", cfg.Workspace.Name) },
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
		PollInterval: time.Second, Actor: task.ActorSnapshot{
			Type: task.ActorSystem, ID: "task-execution", DisplayName: "Task execution observer",
			CredentialID: "service-v1", Authentication: "internal", RequestID: workerID,
		}, RecoveryActor: recoveryActor, Now: time.Now,
		OnError: func(err error) {
			log.Error("task execution observation deferred", "err", err, "workspace", cfg.Workspace.Name)
		},
	})
	if err != nil {
		return nil, err
	}
	var publicationCoordinator *taskpublicationcoord.Coordinator
	if cfg.Workspace.GitHub.Mode == config.GitHubModeGitHubAppBroker {
		publicationTemp := filepath.Join(taskDirectory, cfg.Workspace.Name+"-publication")
		if err := os.MkdirAll(publicationTemp, 0o700); err != nil {
			return nil, fmt.Errorf("create task publication temporary directory: %w", err)
		}
		taskPublisher, publicationErr := taskpublication.New(taskpublication.Config{
			RepositoryPath: cfg.Workspace.Repo, GitExecutable: "/usr/bin/git", TempRoot: publicationTemp,
			Timeout: 2 * time.Minute, OutputLimit: 64 << 10, Now: time.Now,
		}, installationTokens, repositories)
		if publicationErr != nil {
			return nil, publicationErr
		}
		publicationCoordinator, publicationErr = taskpublicationcoord.New(store, manager, taskPublisher, ids, taskpublicationcoord.Config{
			WorkspaceID:      durableWorkspace.ID,
			PullRequestBody:  "Created by Fern from an immutable result after the configured verification policy passed.",
			OperationTimeout: 2 * time.Minute, PollInterval: time.Second,
			Actor: task.ActorSnapshot{
				Type: task.ActorSystem, ID: "task-publication", DisplayName: "Task publication coordinator",
				CredentialID: "service-v1", Authentication: "internal", RequestID: workerID,
			}, RecoveryActor: recoveryActor, Now: time.Now,
			OnError: func(err error) {
				log.Error("task publication reconciliation deferred", "err", err, "workspace", cfg.Workspace.Name)
			},
		})
		if publicationErr != nil {
			return nil, publicationErr
		}
	}
	var verificationCoordinator *taskverification.Coordinator
	if configured := cfg.Tasks.Verification; configured != nil {
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
		verificationCoordinator, err = taskverification.New(store, manager, runner, policy, ids, taskverification.Config{
			WorkspaceID: durableWorkspace.ID, RepositoryPath: cfg.Workspace.Repo,
			PollInterval: time.Second, Deadline: configured.Timeout + 2*time.Minute,
			Actor: task.ActorSnapshot{
				Type: task.ActorSystem, ID: "task-verification", DisplayName: "Task verification coordinator",
				CredentialID: "service-v1", Authentication: "internal", RequestID: workerID,
			}, RecoveryActor: recoveryActor, Now: time.Now,
			OnError: func(err error) { log.Error("task verification deferred", "err", err, "workspace", cfg.Workspace.Name) },
		})
		if err != nil {
			return nil, err
		}
	}
	budget, err := json.Marshal(struct {
		MaxTurns int `json:"maxTurns"`
	}{cfg.Tasks.Budget.MaxTurns})
	if err != nil {
		return nil, err
	}
	handler, err := taskapi.New(taskapi.Config{
		WorkspaceID: durableWorkspace.ID, RepositoryID: durableWorkspace.RepositoryID,
		Store: store, Generator: ids, ActorResolver: taskapi.ContextActor, BaseResolver: baseResolver,
		SealAuthorizer: sealAuthorizer, Wake: coordinator.Wake, SealWake: func() {
			select {
			case resultWake <- struct{}{}:
			default:
			}
		}, Now: time.Now, AttemptTimeout: cfg.Tasks.AttemptTimeout,
		ObjectFormat: "sha1", APIContractVersion: taskAPIContractVersion,
		ExecutionContractVersion: taskExecutionContractVersion, Agent: cfg.Tasks.Agent,
		ModelProvider: cfg.Tasks.Model.Provider, Model: cfg.Tasks.Model.ID, BudgetSnapshot: budget,
	})
	if err != nil {
		return nil, err
	}
	closeOnError = false
	return &taskServices{
		store: store, handler: handler, coordinator: coordinator,
		execution: executionCoordinator, verification: verificationCoordinator, publication: publicationCoordinator,
		result: resultCoordinator, resultWake: resultWake,
	}, nil
}

func runTaskResultCoordinator(ctx context.Context, service *taskServices, log *slog.Logger, workspaceName string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-service.resultWake:
		case <-ticker.C:
		}
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
			log.Error("user-authorized result sealing deferred", "err", err, "workspace", workspaceName)
			break
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
