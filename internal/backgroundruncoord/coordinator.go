// Package backgroundruncoord serially coordinates the one qualified OpenCode
// Background Run profile. It intentionally has no generic runtime abstraction.
package backgroundruncoord

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

var ErrNoWork = errors.New("no background run work")

const sessionDirectory = "/home/user/workspace"

type Config struct {
	WorkspaceID       task.WorkspaceID
	WorkerID          string
	SystemActor       task.ActorSnapshot
	Profile           string
	ImageIdentity     string
	EnvironmentSHA256 [32]byte
	Agent             string
	ModelProvider     string
	Model             string
	OperationTimeout  time.Duration
	LeaseDuration     time.Duration
	PollInterval      time.Duration
	HistoryBounds     backgroundopencode.HistoryBounds
	Now               func() time.Time
	HTTPClient        *http.Client
	Route             *backgroundroute.Manager
	OnError           func(error)
	OnSuccess         func()
	AfterPromptFence  func()
	AfterPromptCall   func(error)
}

type Coordinator struct {
	store      *taskstore.Store
	provider   *taskenvdocker.Provider
	artifact   Artifact
	ids        *task.Generator
	config     Config
	wake       chan struct{}
	scan       sync.Mutex
	terminalMu sync.Mutex
	terminals  map[task.TaskID]*terminalSession
}

// Artifact is the narrow retained-result CAS boundary. StagedLocator and
// Checkout remain opaque engine capabilities and never enter durable state.
type Artifact interface {
	Snapshot(context.Context, taskartifact.SnapshotSpec) (taskartifact.Snapshot, taskartifact.StagedLocator, error)
	StagedManifest(context.Context, taskartifact.StagedLocator) ([]byte, taskartifact.Digest, error)
	Store(context.Context, taskartifact.StagedLocator) (taskartifact.Locator, error)
	Discard(taskartifact.StagedLocator) error
	Inspect(context.Context, taskartifact.Locator) (taskartifact.Snapshot, error)
	Materialize(context.Context, taskartifact.Locator) (*taskartifact.Checkout, error)
}

func New(store *taskstore.Store, provider *taskenvdocker.Provider, artifact Artifact, ids *task.Generator, config Config) (*Coordinator, error) {
	if store == nil || provider == nil || artifact == nil || ids == nil || config.Now == nil || config.HTTPClient == nil || config.Route == nil ||
		config.Profile != taskstore.BackgroundRunSourceProfile || config.ImageIdentity == "" || config.EnvironmentSHA256 == ([32]byte{}) || config.WorkerID == "" ||
		config.Agent == "" || config.ModelProvider == "" || config.Model == "" || config.OperationTimeout <= 0 ||
		config.LeaseDuration <= config.OperationTimeout || config.LeaseDuration > 5*time.Minute || config.PollInterval <= 0 ||
		config.HTTPClient.Timeout <= 0 || config.HTTPClient.Timeout > config.OperationTimeout || config.SystemActor.Validate() != nil ||
		config.SystemActor.Type != task.ActorSystem || config.HistoryBounds.PageLimit < 1 || config.HistoryBounds.MaxPages < 1 ||
		config.HistoryBounds.MaxEvents < 1 {
		return nil, errors.New("valid serial background run coordinator configuration is required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid background run coordinator workspace is required")
	}
	return &Coordinator{store: store, provider: provider, artifact: artifact, ids: ids, config: config, wake: make(chan struct{}, 1), terminals: make(map[task.TaskID]*terminalSession)}, nil
}

func (c *Coordinator) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Coordinator) Run(ctx context.Context) error {
	c.Wake()
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-c.wake:
		}
		err := c.RunOnce(ctx)
		if errors.Is(err, ErrNoWork) {
			if c.config.OnSuccess != nil {
				c.config.OnSuccess()
			}
			continue
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			if c.config.OnError != nil {
				c.config.OnError(err)
			}
			continue
		}
		if c.config.OnSuccess != nil {
			c.config.OnSuccess()
		}
	}
}

// RunOnce performs at most one external lifecycle operation under one durable
// claim. The process-local mutex makes concurrent wake and test scans serial.
func (c *Coordinator) RunOnce(ctx context.Context) error {
	c.scan.Lock()
	defer c.scan.Unlock()
	now, err := c.freshNow()
	if err != nil {
		return err
	}
	control, controlErr := c.store.ClaimNextBackgroundRunControl(ctx, taskstore.ClaimNextBackgroundRunControlParams{
		WorkspaceID: c.config.WorkspaceID, ClaimOwner: c.config.WorkerID, Now: now, LeaseDuration: c.config.LeaseDuration,
	})
	if controlErr == nil {
		work, readErr := c.store.ReadClaimedBackgroundRunControlWork(ctx, controlClaim(control, now))
		if readErr != nil {
			return readErr
		}
		operation, cancel := context.WithTimeout(ctx, c.config.OperationTimeout)
		defer cancel()
		return c.processControl(operation, ctx, work)
	}
	if !errors.Is(controlErr, taskstore.ErrNotFound) {
		return controlErr
	}
	ownership, ownershipErr := c.store.ClaimNextBackgroundRunOwnership(ctx, taskstore.ClaimNextBackgroundRunOwnershipParams{
		WorkspaceID: c.config.WorkspaceID, ClaimOwner: c.config.WorkerID, Now: now, LeaseDuration: c.config.LeaseDuration,
	})
	if ownershipErr == nil {
		work, readErr := c.store.ReadClaimedBackgroundRunOwnershipWork(ctx, ownershipClaim(ownership, now))
		if readErr != nil {
			return readErr
		}
		operation, cancel := context.WithTimeout(ctx, c.config.OperationTimeout)
		defer cancel()
		return c.processOwnership(operation, ctx, work)
	}
	if !errors.Is(ownershipErr, taskstore.ErrNotFound) {
		return ownershipErr
	}
	work, err := c.store.ClaimNextBackgroundRunWork(ctx, taskstore.ClaimNextBackgroundRunParams{
		WorkspaceID: c.config.WorkspaceID, ClaimOwner: c.config.WorkerID, Now: now, LeaseDuration: c.config.LeaseDuration,
		Profile: c.config.Profile, ImageIdentity: c.config.ImageIdentity,
	})
	if errors.Is(err, taskstore.ErrNotFound) {
		return ErrNoWork
	}
	if err != nil {
		return err
	}
	currentOwnership, err := c.store.GetBackgroundRunOwnership(ctx, work.Run.WorkspaceID, work.Run.TaskID)
	if err != nil {
		return err
	}
	work.Run = effectiveRun(work.Run, currentOwnership)
	now, err = c.freshNow()
	if err != nil {
		return err
	}
	if work.Run.CancelEpoch == 0 && work.Run.TimeoutRequestedAt == nil && timeoutState(work.Run.State) &&
		!now.Before(work.Deadline) && !cleanupPhase(work.Run.EffectPhase) {
		return c.requestTimeout(ctx, work.Run)
	}
	configurationDiffers := work.Run.ResourceSpecVersion != 9 || work.Run.ImageIdentity != c.config.ImageIdentity || work.Run.EnvironmentSHA256 != c.config.EnvironmentSHA256 ||
		work.Agent != c.config.Agent || work.ModelProvider != c.config.ModelProvider || work.Model != c.config.Model
	if configurationDiffers && !cleanupPhase(work.Run.EffectPhase) {
		return c.cleanupRequired(ctx, work, "configured execution identity differs")
	}
	operation, cancel, _, err := c.effectContext(ctx, work, !cleanupPhase(work.Run.EffectPhase))
	if err != nil {
		return err
	}
	defer cancel()
	return c.process(operation, ctx, work)
}

func (c *Coordinator) process(ctx, parent context.Context, work taskstore.BackgroundRunWork) error {
	run := work.Run
	switch run.EffectPhase {
	case taskstore.BackgroundRunEffectSealIntent:
		observation, providerFence, err := c.provider.ProveWriterInactive(ctx, run)
		if err != nil {
			return c.retainedFailure(parent, work, fmt.Errorf("prove retained writer inactivity: %w", err))
		}
		if err := c.recordWriterFence(parent, work, observation, providerFence); err != nil {
			return fmt.Errorf("record retained writer fence: %w", err)
		}
		return nil
	case taskstore.BackgroundRunEffectExporting:
		return c.exportRetained(ctx, parent, work)
	case taskstore.BackgroundRunEffectArtifactCommitted:
		return c.record(parent, work, `{"effect":"retained_cleanup_intent","status":"committed"}`, c.store.RequestBackgroundRunResultCleanup)
	case taskstore.BackgroundRunEffectProvisionIntent:
		observation, err := c.provider.EnsureClone(ctx, run)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.record(parent, work, observation.Evidence, c.store.RecordBackgroundRunCloneObserved)
	case taskstore.BackgroundRunEffectCloneObserved:
		observation, err := c.provider.EnsureVolume(ctx, run)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.record(parent, work, observation.Evidence, c.store.RecordBackgroundRunVolumeObserved)
	case taskstore.BackgroundRunEffectVolumeObserved:
		created, err := c.provider.EnsureContainer(ctx, run)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		started, err := c.provider.StartContainer(ctx, run, created.ContainerID)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		mutation, cancel, now, err := c.effectContext(parent, work, true)
		if err != nil {
			return err
		}
		defer cancel()
		_, err = c.store.RecordBackgroundRunContainerObserved(mutation, taskstore.RecordBackgroundRunContainerObservedParams{
			BackgroundRunClaim: claim(run, now), ContainerID: started.ContainerID, ContainerStartedAt: started.ContainerStarted,
			RuntimeEpoch: started.RuntimeEpoch, HostPort: started.HostPort, Evidence: started.Evidence,
		})
		return err
	case taskstore.BackgroundRunEffectContainerObserved:
		runtime, err := c.provider.CommittedRuntime(run)
		if err != nil {
			return c.cleanupRequired(parent, work, "committed runtime identity invalid")
		}
		observation, err := c.provider.Health(ctx, run, runtime)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.record(parent, work, observation.Evidence, c.store.RecordBackgroundRunHealthObserved)
	case taskstore.BackgroundRunEffectHealthObserved:
		return c.record(parent, work, `{"effect":"serial_runtime_ready","status":"exact"}`, c.store.RecordBackgroundRunReady)
	case taskstore.BackgroundRunEffectReady:
		if err := c.ensureRoute(ctx, run); err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.reconcileSession(ctx, parent, work)
	case taskstore.BackgroundRunEffectSessionObserved:
		if err := c.ensureRoute(ctx, run); err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.record(parent, work, `{"effect":"prompt_intent","status":"committed"}`, c.store.RecordBackgroundRunPromptIntent)
	case taskstore.BackgroundRunEffectPromptIntent:
		if err := c.ensureRoute(ctx, run); err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.reconcilePrompt(ctx, parent, work)
	case taskstore.BackgroundRunEffectPromptAdmitted:
		if err := c.ensureRoute(ctx, run); err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.observeWorking(ctx, parent, work)
	case taskstore.BackgroundRunEffectStopIntent:
		observation, _, err := c.provider.ProveWriterInactive(ctx, run)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		if err := c.disconnectTerminalAndWait(ctx, run.TaskID); err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		if _, err := c.provider.RemoveInspector(ctx, run, run.WriterGeneration); err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		return c.record(parent, work, observation.Evidence, c.store.RecordBackgroundRunWriterInactive)
	case taskstore.BackgroundRunEffectWriterInactive:
		if run.BackgroundSealRequestID != "" && run.ResultAuthorityPhase != "cleanup" {
			return c.exportRetained(ctx, parent, work)
		}
		if run.ObservedContainerID == "" && run.ObservedContainerStartedAt == "" && run.RuntimeEpoch == 0 {
			return c.record(parent, work, `{"effect":"route_remove","status":"never_bound"}`, c.store.RecordBackgroundRunRouteRemoved)
		}
		identity, err := c.routeIdentity(run)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		removed, err := c.config.Route.Remove(ctx, identity)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		return c.record(parent, work, removed, c.store.RecordBackgroundRunRouteRemoved)
	case taskstore.BackgroundRunEffectRouteRemoved:
		if run.ObservedContainerID != "" || run.ObservedContainerStartedAt != "" || run.RuntimeEpoch != 0 {
			identity, identityErr := c.routeIdentity(run)
			if identityErr != nil {
				return c.cleanupFailure(parent, work, identityErr)
			}
			if err := c.config.Route.ConfirmRemoval(identity); err != nil {
				return c.cleanupFailure(parent, work, err)
			}
		}
		_, authority, err := c.provider.ProveWriterInactive(ctx, run)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		observation, err := c.provider.RemoveContainer(ctx, run, authority)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		return c.record(parent, work, observation.Evidence, c.store.RecordBackgroundRunContainerRemoved)
	case taskstore.BackgroundRunEffectContainerRemoved:
		_, authority, err := c.provider.ProveWriterInactive(ctx, run)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		observation, err := c.provider.RemoveVolume(ctx, run, authority)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		return c.record(parent, work, observation.Evidence, c.store.RecordBackgroundRunVolumeRemoved)
	case taskstore.BackgroundRunEffectVolumeRemoved:
		_, authority, err := c.provider.ProveWriterInactive(ctx, run)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		observation, err := c.provider.RemoveClone(ctx, run, authority)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		return c.record(parent, work, observation.Evidence, c.store.RecordBackgroundRunCloneRemoved)
	case taskstore.BackgroundRunEffectCloneRemoved:
		if run.BackgroundSealRequestID != "" {
			mutation, cancel, now, err := c.effectContext(parent, work, false)
			if err != nil {
				return err
			}
			defer cancel()
			_, err = c.store.CompleteBackgroundRunResultCleanup(mutation, taskstore.CompleteBackgroundRunResultCleanupParams{
				BackgroundRunClaim: claim(run, now), CleanupProof: `{"route":"absent","container":"absent","volume":"absent","clone":"absent"}`})
			return err
		}
		attemptEvent, err := c.ids.EventID()
		if err != nil {
			return err
		}
		taskEvent, err := c.ids.EventID()
		if err != nil {
			return err
		}
		reason := "runtime_unavailable"
		if run.TimeoutRequestedAt != nil {
			reason = "attempt_timeout"
		} else if run.CancelEpoch == 1 {
			reason = "user_stopped"
		}
		mutation, cancel, now, err := c.effectContext(parent, work, false)
		if err != nil {
			return err
		}
		defer cancel()
		actor := c.config.SystemActor
		if run.TimeoutRequestedAt != nil {
			if run.TimeoutActor == nil {
				return taskstore.ErrCorruptStore
			}
			actor = *run.TimeoutActor
		}
		_, err = c.store.FinalizeBackgroundRunFailure(mutation, taskstore.FinalizeBackgroundRunFailureParams{
			BackgroundRunClaim: claim(run, now), AttemptEventID: attemptEvent, TaskEventID: taskEvent, Actor: actor,
			Reason: reason, Evidence: `{"effect":"terminalize","status":"resources_absent"}`,
			CleanupProof: `{"route":"serial_absent","container":"absent","volume":"absent","clone":"absent"}`,
		})
		return err
	default:
		mutation, cancel, now, err := c.effectContext(parent, work, !cleanupPhase(run.EffectPhase))
		if err != nil {
			return err
		}
		defer cancel()
		_, err = c.store.ReleaseBackgroundRunClaim(mutation, claim(run, now))
		return err
	}
}

func (c *Coordinator) recordWriterFence(ctx context.Context, work taskstore.BackgroundRunWork, observation taskenvdocker.Observation, provider taskenvdocker.WriterFence) error {
	run := work.Run
	now, err := c.freshNow()
	if err != nil {
		return err
	}
	params := taskstore.RecordBackgroundRunWriterFenceParams{BackgroundRunClaim: claim(run, now),
		SealRequestID: run.BackgroundSealRequestID, ExportID: run.ArtifactExportID}
	switch {
	case provider.NeverCreated:
		params.Kind = taskstore.WriterFenceNeverCreated
	case provider.StartedAt == "":
		params.Kind, params.ContainerID = taskstore.WriterFenceNeverStarted, provider.ContainerID
	default:
		params.Kind, params.ContainerID, params.ContainerStartedAt = taskstore.WriterFenceRuntimeStopped, provider.ContainerID, provider.StartedAt
		params.RuntimeEpoch, params.RuntimeToken = run.RuntimeEpoch, provider.Token
		params.StoppedAt = &now
	}
	params.ProofSHA256, err = taskstore.WriterFenceProofDigest(params)
	if err != nil {
		return err
	}
	mutation, cancel, _, err := c.effectContext(ctx, work, false)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.RecordBackgroundRunWriterFence(mutation, params)
	if err != nil {
		return err
	}
	_ = observation // Structured fence, not provider prose, is durable authority.
	return nil
}

func (c *Coordinator) exportRetained(ctx, parent context.Context, work taskstore.BackgroundRunWork) (resultErr error) {
	run := work.Run
	export, err := c.store.GetBackgroundRunExport(parent, run.ArtifactExportID)
	if err != nil {
		return c.retainedFailure(parent, work, err)
	}
	now, err := c.freshNow()
	if err != nil {
		return err
	}
	export, err = c.store.ClaimBackgroundRunExport(parent, taskstore.ClaimBackgroundRunExportParams{
		ExportID: export.ID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRevision: export.Revision, ExpectedPhase: export.Phase, ClaimOwner: c.config.WorkerID, Now: now, LeaseDuration: c.config.LeaseDuration,
	})
	if err != nil {
		return c.retainedFailure(parent, work, fmt.Errorf("claim retained export: %w", err))
	}
	claimExport := func() taskstore.BackgroundRunExportClaim {
		n, _ := c.freshNow()
		return taskstore.BackgroundRunExportClaim{ExportID: export.ID, TaskID: export.TaskID, AttemptID: export.AttemptID,
			Generation: export.Generation, ExpectedRevision: export.Revision, ExpectedPhase: export.Phase,
			ClaimOwner: export.ClaimOwner, ClaimGeneration: export.ClaimGeneration, Now: n}
	}
	fail := func(cause error) error {
		claim := claimExport()
		_, markErr := c.store.MarkBackgroundRunExportRecoveryRequired(context.WithoutCancel(parent), claim, "retained artifact export retry required")
		var releaseErr error
		if markErr == nil {
			_, releaseErr = c.store.ReleaseBackgroundRunClaimAfterExportFailure(context.WithoutCancel(parent), claim)
		}
		return errors.Join(cause, markErr, releaseErr)
	}

	locator, locatorErr := taskartifact.ParseLocator(export.CASLocator)
	if export.Phase == taskstore.BackgroundRunExportPhaseCASInstallStarted && locatorErr == nil {
		if snapshot, inspectErr := c.artifact.Inspect(ctx, locator); inspectErr == nil && snapshotMatchesExport(snapshot, export) {
			export, err = c.store.RecordBackgroundRunCASInstalled(parent, claimExport())
			if err != nil {
				return fail(err)
			}
		}
	}

	if export.Phase == taskstore.BackgroundRunExportPhasePrepared || export.Phase == taskstore.BackgroundRunExportPhaseSnapshotStarted ||
		export.Phase == taskstore.BackgroundRunExportPhaseSnapshotSelected || export.Phase == taskstore.BackgroundRunExportPhaseBundleWriteStarted ||
		export.Phase == taskstore.BackgroundRunExportPhaseBundleVerified || export.Phase == taskstore.BackgroundRunExportPhaseCASInstallStarted {
		fence, fenceErr := c.store.GetBackgroundRunWriterFence(parent, run.BackgroundSealRequestID)
		if fenceErr != nil {
			return fail(fenceErr)
		}
		source, sourceErr := c.provider.AcquireExportSource(ctx, run, providerFence(fence))
		if sourceErr != nil {
			return fail(sourceErr)
		}
		defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
		if export.Phase == taskstore.BackgroundRunExportPhasePrepared {
			export, err = c.store.RecordBackgroundRunSnapshotStarted(parent, claimExport())
			if err != nil {
				return fail(err)
			}
		}
		request, requestErr := c.store.GetBackgroundRunSealRequest(parent, run.BackgroundSealRequestID)
		if requestErr != nil {
			return fail(requestErr)
		}
		artifactSource, sourceSpecErr := taskartifact.NewSource(source.RepositoryPath(), run.WorkspaceID, run.TaskID, run.AttemptID)
		profileDigest, profileErr := taskartifact.NewDigest(run.ProfileSHA256)
		environmentDigest, environmentErr := taskartifact.NewDigest(run.EnvironmentSHA256)
		if sourceSpecErr != nil || profileErr != nil || environmentErr != nil {
			return fail(errors.Join(sourceSpecErr, profileErr, environmentErr))
		}
		snapshot, staged, snapshotErr := c.artifact.Snapshot(ctx, taskartifact.SnapshotSpec{
			Source: artifactSource, RepositoryID: run.RepositoryID, Generation: run.Generation, SealRequestID: run.BackgroundSealRequestID,
			ImageIdentity: run.ImageIdentity, Profile: run.Profile, ProfileSHA256: profileDigest, EnvironmentSHA256: environmentDigest,
			ResourceSpecVersion: taskartifact.ResourceSpecVersion, OpenCodeSessionID: run.OpenCodeSessionID, OpenCodeMessageID: run.OpenCodeMessageID,
			SnapshotPolicyVersion: taskartifact.SnapshotPolicyV1, Base: run.BaseOID, EpochSecond: request.CommitEpochSeconds,
		})
		if snapshotErr != nil {
			return fail(snapshotErr)
		}
		stored := false
		defer func() {
			if !stored {
				resultErr = errors.Join(resultErr, c.artifact.Discard(staged))
			}
		}()
		manifestBytes, manifestDigest, manifestErr := c.artifact.StagedManifest(ctx, staged)
		if manifestErr != nil {
			return fail(manifestErr)
		}
		entries := manifestEntries(snapshot.Changes)
		if export.Phase == taskstore.BackgroundRunExportPhaseSnapshotStarted {
			outcome := task.ResultChanged
			if snapshot.Result == snapshot.Base {
				outcome = task.ResultNoChanges
			}
			export, err = c.store.SelectBackgroundRunSnapshot(parent, taskstore.SelectBackgroundRunSnapshotParams{
				BackgroundRunExportClaim: claimExport(), ResultCommit: snapshot.Result, TreeOID: snapshot.Tree, Outcome: outcome,
				ResultManifest: entries, ChangesSHA256: snapshot.ChangesSHA256.Bytes(), ArtifactManifest: manifestBytes,
				ArtifactManifestSHA256: manifestDigest.Bytes(), OpenCodeSessionID: snapshot.OpenCodeSessionID,
				OpenCodeMessageID: snapshot.OpenCodeMessageID, CollectedAt: now,
			})
			if err != nil {
				return fail(err)
			}
		} else if !snapshotMatchesExport(snapshot, export) {
			return fail(errors.New("replayed retained snapshot differs from durable selection"))
		}
		if export.Phase == taskstore.BackgroundRunExportPhaseSnapshotSelected {
			export, err = c.store.RecordBackgroundRunBundleWriteStarted(parent, claimExport())
			if err != nil {
				return fail(err)
			}
		}
		if export.Phase == taskstore.BackgroundRunExportPhaseBundleWriteStarted {
			export, err = c.store.VerifyBackgroundRunBundle(parent, taskstore.VerifyBackgroundRunBundleParams{
				BackgroundRunExportClaim: claimExport(), BundleSHA256: snapshot.BundleSHA256.Bytes(), BundleBytes: snapshot.BundleBytes})
			if err != nil {
				return fail(err)
			}
		}
		if export.Phase == taskstore.BackgroundRunExportPhaseBundleVerified {
			export, err = c.store.RecordBackgroundRunCASInstallStarted(parent, claimExport())
			if err != nil {
				return fail(err)
			}
		}
		locator, err = c.artifact.Store(ctx, staged)
		if err != nil {
			return fail(err)
		}
		stored = true
		inspected, inspectErr := c.artifact.Inspect(ctx, locator)
		if inspectErr != nil || !snapshotMatchesExport(inspected, export) || locator.String() != export.CASLocator {
			return fail(errors.Join(inspectErr, errors.New("installed retained artifact differs from durable selection")))
		}
		export, err = c.store.RecordBackgroundRunCASInstalled(parent, claimExport())
		if err != nil {
			return fail(err)
		}
	}

	locator, err = taskartifact.ParseLocator(export.CASLocator)
	if err != nil {
		return fail(err)
	}
	if export.Phase == taskstore.BackgroundRunExportPhaseCASInstalled {
		export, err = c.store.RecordBackgroundRunMaterializeStarted(parent, claimExport())
		if err != nil {
			return fail(err)
		}
	}
	if export.Phase == taskstore.BackgroundRunExportPhaseMaterializeStarted {
		checkout, materializeErr := c.artifact.Materialize(ctx, locator)
		if materializeErr != nil {
			return fail(materializeErr)
		}
		path := checkout.Path()
		proof := materializationProof(export, path)
		closeErr := checkout.Close()
		if closeErr != nil {
			return fail(closeErr)
		}
		export, err = c.store.RecordArtifactMaterializationReady(parent, taskstore.RecordArtifactMaterializationReadyParams{
			BackgroundRunExportClaim: claimExport(), MaterializationID: export.MaterializationID, ArtifactID: export.ArtifactID,
			ResultID: export.ResultID, ResultCommit: export.ResultCommit, TreeOID: export.TreeOID, ProofSHA256: proof})
		if err != nil {
			return fail(err)
		}
	}
	if export.Phase == taskstore.BackgroundRunExportPhaseMaterialized {
		request, requestErr := c.store.GetBackgroundRunSealRequest(parent, run.BackgroundSealRequestID)
		if requestErr != nil {
			return fail(requestErr)
		}
		sealedAt, timeErr := c.freshNow()
		if timeErr != nil {
			return fail(timeErr)
		}
		evidence, _ := json.Marshal(struct{ Schema, Locator string }{"fern.background-retained-result.v1", export.CASLocator})
		_, err = c.store.CommitBackgroundRunRetainedResult(context.WithoutCancel(parent), taskstore.CommitBackgroundRunRetainedResultParams{
			BackgroundRunExportClaim: claimExport(), MaterializationID: export.MaterializationID, ArtifactID: export.ArtifactID,
			ResultID: export.ResultID, ResultEventID: request.ResultEventID, TaskEventID: request.TaskEventID,
			EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: c.config.SystemActor, SealedAt: sealedAt,
		})
		if err != nil {
			return fail(err)
		}
	}
	return nil
}

func providerFence(value taskstore.WriterFence) taskenvdocker.WriterFence {
	switch value.Kind {
	case taskstore.WriterFenceNeverCreated:
		return taskenvdocker.NeverCreatedAuthority()
	case taskstore.WriterFenceNeverStarted:
		return taskenvdocker.CreatedContainerAuthority(value.ContainerID)
	default:
		return taskenvdocker.RuntimeCleanupAuthority(taskenvdocker.RuntimeIdentity{ContainerID: value.ContainerID, StartedAt: value.ContainerStartedAt, Token: value.RuntimeToken})
	}
}

func manifestEntries(changes []taskartifact.ChangeEntry) []taskstore.ManifestEntry {
	result := make([]taskstore.ManifestEntry, len(changes))
	for i, change := range changes {
		result[i] = taskstore.ManifestEntry{PathBase64: change.PathBase64, ChangeKind: change.Kind}
		if change.Old != nil {
			mode, oid, size := change.Old.Mode, string(change.Old.BlobOID), change.Old.Size
			result[i].OldMode, result[i].OldBlobOID, result[i].OldSize = &mode, &oid, &size
		}
		if change.New != nil {
			mode, oid, size := change.New.Mode, string(change.New.BlobOID), change.New.Size
			result[i].NewMode, result[i].NewBlobOID, result[i].NewSize = &mode, &oid, &size
		}
	}
	return result
}

func snapshotMatchesExport(snapshot taskartifact.Snapshot, export taskstore.BackgroundRunExport) bool {
	bundleMatches := export.BundleSHA256 == ([32]byte{}) || snapshot.BundleSHA256.Bytes() == export.BundleSHA256 && snapshot.BundleBytes == export.BundleBytes
	return snapshot.RepositoryID == export.RepositoryID && snapshot.WorkspaceID == export.WorkspaceID && snapshot.TaskID == export.TaskID &&
		snapshot.AttemptID == export.AttemptID && snapshot.Generation == export.Generation && snapshot.SealRequestID == export.SealRequestID &&
		snapshot.Base == export.BaseSHA && snapshot.Result == export.ResultCommit && snapshot.Tree == export.TreeOID &&
		snapshot.ChangesSHA256.Bytes() == export.ChangesSHA256 && snapshot.ManifestSHA256.Bytes() == export.ArtifactManifestSHA256 && bundleMatches
}

func materializationProof(export taskstore.BackgroundRunExport, path string) [32]byte {
	// Path is deliberately reduced to an observation bit; host paths never enter
	// durable evidence. Engine.Materialize already proves detached clean state.
	payload, _ := json.Marshal(struct {
		Schema  string      `json:"schema"`
		Locator string      `json:"locator"`
		Commit  task.GitOID `json:"commit"`
		Tree    task.GitOID `json:"tree"`
		Clean   bool        `json:"clean"`
	}{"fern.taskartifact.materialization.v1", export.CASLocator, export.ResultCommit, export.TreeOID, path != ""})
	return sha256.Sum256(payload)
}

func (c *Coordinator) retainedFailure(ctx context.Context, work taskstore.BackgroundRunWork, external error) error {
	mutation, cancel, now, err := c.effectContext(ctx, work, false)
	if err != nil {
		return errors.Join(external, err)
	}
	defer cancel()
	_, releaseErr := c.store.ReleaseBackgroundRunClaim(mutation, claim(work.Run, now))
	return errors.Join(external, releaseErr)
}

func (c *Coordinator) reconcileSession(ctx, parent context.Context, work taskstore.BackgroundRunWork) error {
	run := work.Run
	client, err := c.client(ctx, run)
	if err != nil {
		return c.externalFailure(parent, work, err)
	}
	spec := backgroundopencode.SessionSpec{ID: string(run.OpenCodeSessionID), Agent: c.config.Agent,
		ProviderID: c.config.ModelProvider, ModelID: c.config.Model, Directory: sessionDirectory}
	state, err := client.ReconcileSession(ctx, spec)
	if err == nil && state == backgroundopencode.ReconcileAbsent {
		createErr := client.CreateSessionOnce(ctx, spec)
		state, err = client.ReconcileSession(ctx, spec)
		if err == nil && state == backgroundopencode.ReconcileAbsent {
			if createErr == nil {
				return c.cleanupRequired(parent, work, "OpenCode session disappeared after creation")
			}
			return errors.Join(createErr, c.cleanupRequired(parent, work, "OpenCode session creation is inconclusive"))
		}
	}
	if err != nil {
		return c.externalFailure(parent, work, err)
	}
	if state == backgroundopencode.ReconcileConflict {
		return c.cleanupRequired(parent, work, "OpenCode session identity conflict")
	}
	if state != backgroundopencode.ReconcileExact {
		mutation, cancel, now, mutationErr := c.effectContext(parent, work, true)
		if mutationErr != nil {
			return mutationErr
		}
		defer cancel()
		_, releaseErr := c.store.ReleaseBackgroundRunClaim(mutation, claim(run, now))
		return releaseErr
	}
	return c.record(parent, work, `{"effect":"session_reconcile","status":"exact"}`, c.store.RecordBackgroundRunSessionObserved)
}

func (c *Coordinator) reconcilePrompt(ctx, parent context.Context, work taskstore.BackgroundRunWork) error {
	run := work.Run
	client, err := c.client(ctx, run)
	if err != nil {
		recordErr := c.record(parent, work, `{"effect":"prompt_reconcile","status":"runtime_unavailable"}`, c.store.RecordBackgroundRunPromptUncertain)
		return errors.Join(err, recordErr)
	}
	spec := backgroundopencode.PromptSpec{ID: string(run.OpenCodeMessageID), Text: work.Prompt, Resume: true, Delivery: "steer"}
	if run.PromptRequestAttemptedAt == nil {
		mutation, cancel, now, mutationErr := c.effectContext(parent, work, true)
		if mutationErr != nil {
			return mutationErr
		}
		defer cancel()
		if !now.Before(work.Deadline) {
			return c.requestTimeout(parent, run)
		}
		run, err = c.store.RecordBackgroundRunPromptRequestAttempted(mutation, claim(run, now))
		if err != nil {
			return err
		}
		work.Run = run
		if c.config.AfterPromptFence != nil {
			c.config.AfterPromptFence()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		dispatchErr := c.promptDispatchAuthority(work)
		if errors.Is(dispatchErr, context.DeadlineExceeded) {
			return c.requestTimeout(parent, run)
		}
		if dispatchErr != nil {
			return dispatchErr
		}
		callErr := client.AdmitPromptOnce(ctx, string(run.OpenCodeSessionID), spec)
		if c.config.AfterPromptCall != nil {
			c.config.AfterPromptCall(callErr)
		}
	}
	if err := parent.Err(); err != nil {
		return err
	}
	reconcileCtx, reconcileCancel, _, contextErr := c.effectContext(parent, work, true)
	if contextErr != nil {
		return contextErr
	}
	defer reconcileCancel()
	state, reconcileErr := client.ReconcilePrompt(reconcileCtx, string(run.OpenCodeSessionID), spec, c.config.HistoryBounds)
	if reconcileErr == nil && state == backgroundopencode.ReconcileExact {
		return c.record(parent, work, `{"effect":"prompt_reconcile","status":"admitted_promoted"}`, c.store.RecordBackgroundRunPromptAdmitted)
	}
	status := "inconclusive"
	if reconcileErr == nil {
		status = string(state)
	}
	return c.record(parent, work, fmt.Sprintf(`{"effect":"prompt_reconcile","status":%q}`, status), c.store.RecordBackgroundRunPromptUncertain)
}

func (c *Coordinator) observeWorking(ctx, parent context.Context, work taskstore.BackgroundRunWork) error {
	run := work.Run
	usage, err := c.provider.ObserveUsage(ctx, run)
	if err != nil {
		if errors.Is(err, taskenvdocker.ErrIdentityMismatch) || errors.Is(err, taskenvdocker.ErrQuarantined) {
			return errors.Join(err, c.cleanupRequired(parent, work, "background usage limit or identity mismatch"))
		}
		return c.externalFailure(parent, work, err)
	}
	client, err := c.client(ctx, run)
	if err != nil {
		recordErr := c.recordObservation(parent, work, `{"effect":"work_observe","status":"runtime_unavailable"}`, taskstore.BackgroundRunUncertain)
		return errors.Join(err, recordErr)
	}
	observation, err := client.ObservePending(ctx, string(run.OpenCodeSessionID))
	if err != nil {
		recordErr := c.recordObservation(parent, work, `{"effect":"work_observe","status":"inconclusive"}`, taskstore.BackgroundRunUncertain)
		return errors.Join(err, recordErr)
	}
	state := taskstore.BackgroundRunState("")
	status := ""
	if observation.Questions > 0 || observation.Permissions > 0 {
		state, status = taskstore.BackgroundRunNeedsYou, "owned_pending"
	} else if observation.Active {
		state, status = taskstore.BackgroundRunWorking, "positive_active"
	}
	if state == "" {
		mutation, cancel, now, mutationErr := c.effectContext(parent, work, true)
		if mutationErr != nil {
			return mutationErr
		}
		defer cancel()
		_, err = c.store.ReleaseBackgroundRunClaim(mutation, claim(run, now))
		return err
	}
	value := fmt.Sprintf(`{"effect":"work_observe","status":%q,"questions":%d,"permissions":%d,"usage":%s}`,
		status, observation.Questions, observation.Permissions, usage.Evidence)
	return c.recordObservation(parent, work, value, state)
}

func (c *Coordinator) client(ctx context.Context, run taskstore.BackgroundRun) (*backgroundopencode.Client, error) {
	runtime, err := c.provider.CommittedRuntime(run)
	if err != nil {
		return nil, err
	}
	if _, err := c.provider.Health(ctx, run, runtime); err != nil {
		return nil, err
	}
	return c.provider.OpenCodeClient(run, runtime, c.config.HTTPClient)
}

func (c *Coordinator) ensureRoute(ctx context.Context, run taskstore.BackgroundRun) error {
	runtime, err := c.provider.CommittedRuntime(run)
	if err != nil {
		return err
	}
	if _, err := c.provider.Health(ctx, run, runtime); err != nil {
		return err
	}
	target, err := c.provider.BackgroundRouteTarget(run, runtime)
	if err != nil {
		return err
	}
	_, err = c.config.Route.Activate(routeIdentity(run, runtime), target)
	return err
}

func (c *Coordinator) routeIdentity(run taskstore.BackgroundRun) (backgroundroute.Identity, error) {
	runtime, err := c.provider.CommittedRuntime(run)
	if err != nil {
		return backgroundroute.Identity{}, err
	}
	return routeIdentity(run, runtime), nil
}

func routeIdentity(run taskstore.BackgroundRun, runtime taskenvdocker.RuntimeIdentity) backgroundroute.Identity {
	writerGeneration := run.WriterGeneration
	if writerGeneration < 1 {
		writerGeneration = 1
	}
	return backgroundroute.Identity{WorkspaceID: string(run.WorkspaceID), TaskID: string(run.TaskID), AttemptID: string(run.AttemptID),
		Generation: run.Generation, WriterGeneration: writerGeneration, Role: "agent", RuntimeEpoch: run.RuntimeEpoch, ContainerID: runtime.ContainerID, StartedAt: runtime.StartedAt, RuntimeToken: runtime.Token}
}

func (c *Coordinator) externalFailure(ctx context.Context, work taskstore.BackgroundRunWork, external error) error {
	if errors.Is(external, taskenvdocker.ErrIdentityMismatch) || errors.Is(external, taskenvdocker.ErrQuarantined) {
		return errors.Join(external, c.cleanupRequired(ctx, work, "background resource identity mismatch"))
	}
	mutation, cancel, now, mutationErr := c.effectContext(ctx, work, !cleanupPhase(work.Run.EffectPhase))
	if mutationErr != nil {
		return errors.Join(external, mutationErr)
	}
	defer cancel()
	_, releaseErr := c.store.ReleaseBackgroundRunClaim(mutation, claim(work.Run, now))
	return errors.Join(external, releaseErr)
}

func (c *Coordinator) cleanupRequired(ctx context.Context, work taskstore.BackgroundRunWork, reason string) error {
	if identity, identityErr := c.routeIdentity(work.Run); identityErr == nil && c.config.Route.Active(identity) {
		if _, removeErr := c.config.Route.Remove(ctx, identity); removeErr != nil {
			return removeErr
		}
	}
	mutation, cancel, now, err := c.effectContext(ctx, work, !cleanupPhase(work.Run.EffectPhase))
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.MarkBackgroundRunCleanupRequired(mutation, taskstore.MarkBackgroundRunCleanupRequiredParams{
		BackgroundRunClaim: claim(work.Run, now), Error: reason,
	})
	return err
}

func (c *Coordinator) cleanupFailure(ctx context.Context, work taskstore.BackgroundRunWork, external error) error {
	return errors.Join(external, c.cleanupRequired(ctx, work, "background cleanup retry required"))
}

func (c *Coordinator) freshNow() (time.Time, error) {
	raw := c.config.Now()
	if raw.IsZero() || raw.UnixMilli() < 0 {
		return time.Time{}, errors.New("background run clock returned an invalid timestamp")
	}
	return raw.UTC().Truncate(time.Millisecond), nil
}

func (c *Coordinator) promptDispatchAuthority(work taskstore.BackgroundRunWork) error {
	now, err := c.freshNow()
	if err != nil {
		return err
	}
	if !now.Before(work.Deadline) {
		return context.DeadlineExceeded
	}
	if work.Run.ClaimExpiresAt == nil || !work.Run.ClaimExpiresAt.After(now) {
		return taskstore.ErrInvalidState
	}
	return nil
}

func (c *Coordinator) effectContext(parent context.Context, work taskstore.BackgroundRunWork, enforceAttemptDeadline bool) (context.Context, context.CancelFunc, time.Time, error) {
	now, err := c.freshNow()
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if work.Run.ClaimExpiresAt == nil {
		return nil, nil, time.Time{}, taskstore.ErrInvalidState
	}
	deadline := now.Add(c.config.OperationTimeout)
	if work.Run.ClaimExpiresAt.Before(deadline) {
		deadline = *work.Run.ClaimExpiresAt
	}
	if enforceAttemptDeadline && work.Deadline.Before(deadline) {
		deadline = work.Deadline
	}
	if !deadline.After(now) {
		return nil, nil, now, context.DeadlineExceeded
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	return ctx, cancel, now, nil
}

func (c *Coordinator) requestTimeout(ctx context.Context, run taskstore.BackgroundRun) error {
	attemptEvent, err := c.ids.EventID()
	if err != nil {
		return err
	}
	taskEvent, err := c.ids.EventID()
	if err != nil {
		return err
	}
	work := taskstore.BackgroundRunWork{Run: run}
	mutation, cancel, now, err := c.effectContext(ctx, work, false)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.RequestBackgroundRunTimeout(mutation, taskstore.RequestBackgroundRunTimeoutParams{
		BackgroundRunClaim: claim(run, now), AttemptEventID: attemptEvent, TaskEventID: taskEvent, Actor: c.config.SystemActor,
	})
	return err
}

func (c *Coordinator) record(ctx context.Context, work taskstore.BackgroundRunWork, value string,
	transition func(context.Context, taskstore.RecordBackgroundRunEvidenceParams) (taskstore.BackgroundRun, error)) error {
	mutation, cancel, now, err := c.effectContext(ctx, work, !cleanupPhase(work.Run.EffectPhase))
	if err != nil {
		return err
	}
	defer cancel()
	_, err = transition(mutation, evidence(work.Run, now, value))
	return err
}

func (c *Coordinator) recordObservation(ctx context.Context, work taskstore.BackgroundRunWork, value string, state taskstore.BackgroundRunState) error {
	mutation, cancel, now, err := c.effectContext(ctx, work, true)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.RecordBackgroundRunWorkObservation(mutation, evidence(work.Run, now, value), state)
	return err
}

func claim(run taskstore.BackgroundRun, now time.Time) taskstore.BackgroundRunClaim {
	return taskstore.BackgroundRunClaim{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID,
		Generation: run.Generation, ClaimOwner: run.ClaimOwner, ClaimGeneration: run.ClaimGeneration, ExpectedRevision: run.Revision,
		ExpectedState: run.State, ExpectedPhase: run.EffectPhase, CancelEpoch: run.CancelEpoch, Now: now}
}

func evidence(run taskstore.BackgroundRun, now time.Time, value string) taskstore.RecordBackgroundRunEvidenceParams {
	return taskstore.RecordBackgroundRunEvidenceParams{BackgroundRunClaim: claim(run, now), Evidence: value}
}

func cleanupPhase(phase taskstore.BackgroundRunEffectPhase) bool {
	return phase == taskstore.BackgroundRunEffectStopIntent || phase == taskstore.BackgroundRunEffectWriterInactive ||
		phase == taskstore.BackgroundRunEffectSealIntent || phase == taskstore.BackgroundRunEffectExporting || phase == taskstore.BackgroundRunEffectArtifactCommitted ||
		phase == taskstore.BackgroundRunEffectRouteRemoved || phase == taskstore.BackgroundRunEffectContainerRemoved ||
		phase == taskstore.BackgroundRunEffectVolumeRemoved || phase == taskstore.BackgroundRunEffectCloneRemoved ||
		phase == taskstore.BackgroundRunEffectCleanupComplete || phase == taskstore.BackgroundRunEffectPreEffectFailed
}

func timeoutState(state taskstore.BackgroundRunState) bool {
	return state == taskstore.BackgroundRunSettingUp || state == taskstore.BackgroundRunWorking ||
		state == taskstore.BackgroundRunNeedsYou || state == taskstore.BackgroundRunUncertain
}
