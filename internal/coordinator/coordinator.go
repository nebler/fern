package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nebler/fern/internal/artifact"
	"github.com/nebler/fern/internal/docker"
	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/opencode"
	"github.com/nebler/fern/internal/store"
)

var ErrNoWork = errors.New("no background run work")

const sessionDirectory = "/home/user/workspace"

const (
	readinessMinInterval = 50 * time.Millisecond
	readinessMaxInterval = 250 * time.Millisecond
)

type Config struct {
	WorkspaceID       domain.WorkspaceID
	Profile           string
	ImageIdentity     string
	EnvironmentSHA256 [32]byte
	Agent             string
	ModelProvider     string
	Model             string
	OperationTimeout  time.Duration
	PollInterval      time.Duration
	HistoryBounds     opencode.HistoryBounds
	Now               func() time.Time
	HTTPClient        *http.Client
	Route             *opencode.Router
	OnError           func(error)
	OnSuccess         func()
	AfterPromptFence  func()
	AfterPromptCall   func(error)
}

type Coordinator struct {
	store    *store.Store
	provider *docker.Provider
	artifact Artifact
	ids      *domain.Generator
	config   Config
	wake     chan struct{}
	scan     sync.Mutex
}

// Artifact is the narrow retained-result CAS boundary. StagedLocator and
// Checkout remain opaque engine capabilities and never enter durable state.
type Artifact interface {
	Snapshot(context.Context, artifact.SnapshotSpec) (artifact.Snapshot, artifact.StagedLocator, error)
	StagedManifest(context.Context, artifact.StagedLocator) ([]byte, artifact.Digest, error)
	Store(context.Context, artifact.StagedLocator) (artifact.Locator, error)
	Discard(artifact.StagedLocator) error
	Inspect(context.Context, artifact.Locator) (artifact.Snapshot, error)
	Materialize(context.Context, artifact.Locator) (*artifact.Checkout, error)
}

func New(runStore *store.Store, provider *docker.Provider, engine Artifact, ids *domain.Generator, config Config) (*Coordinator, error) {
	if runStore == nil || provider == nil || engine == nil || ids == nil || config.Now == nil || config.HTTPClient == nil || config.Route == nil ||
		config.Profile != store.BackgroundRunSourceProfile || config.ImageIdentity == "" || config.EnvironmentSHA256 == ([32]byte{}) ||
		config.Agent == "" || config.ModelProvider == "" || config.Model == "" || config.OperationTimeout <= 0 ||
		config.OperationTimeout > 5*time.Minute || config.PollInterval <= 0 ||
		config.HTTPClient.Timeout <= 0 || config.HTTPClient.Timeout > config.OperationTimeout ||
		config.HistoryBounds.PageLimit < 1 || config.HistoryBounds.MaxPages < 1 ||
		config.HistoryBounds.MaxEvents < 1 {
		return nil, errors.New("valid serial background run coordinator configuration is required")
	}
	if _, err := domain.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid background run coordinator workspace is required")
	}
	return &Coordinator{store: runStore, provider: provider, artifact: engine, ids: ids, config: config, wake: make(chan struct{}, 1)}, nil
}

func (c *Coordinator) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Coordinator) Run(ctx context.Context) error {
	return c.supervise(ctx, c.step)
}

// supervise owns the policy for scan outcomes: corruption terminates the
// component, cancellation stops it, and transient failures may be retried.
// A scan that advanced its run's lifecycle runs again immediately; idle,
// failed, and steady-state scans wait for the next tick or wake.
func (c *Coordinator) supervise(ctx context.Context, step func(context.Context) (bool, error)) error {
	c.Wake()
	ticker := time.NewTicker(c.config.PollInterval)
	defer ticker.Stop()
	progressed := false
	for {
		if progressed {
			if err := ctx.Err(); err != nil {
				return err
			}
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			case <-c.wake:
			}
		}
		var err error
		progressed, err = step(ctx)
		if errors.Is(err, store.ErrCorruptStore) {
			return err
		}
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

// RunOnce selects the next run and reconciles its current phase in one pass:
// it observes each derived resource, acts where it differs, and continues
// until the phase is blocked, stable, or the operation deadline expires. Every
// durable write is a revision compare-and-swap. The process-local mutex makes
// concurrent wake and test scans serial; fern up's host lease makes this the
// workspace's only coordinator.
func (c *Coordinator) RunOnce(ctx context.Context) error {
	_, err := c.step(ctx)
	return err
}

// step runs one scan and reports whether it advanced the selected run's durable
// lifecycle. Observation of a working run, deferred retries, and repeated
// uncertain reconciliation do not count as progress, so the supervisor cannot
// busy-loop on them.
func (c *Coordinator) step(ctx context.Context) (bool, error) {
	c.scan.Lock()
	defer c.scan.Unlock()
	work, err := c.runOnce(ctx)
	if err != nil {
		return false, err
	}
	state, phase, readErr := c.store.ReadBackgroundRunLifecycle(ctx, work.Run.WorkspaceID, work.Run.RunID)
	if readErr != nil {
		return false, nil
	}
	// A working run's state may change on observation without new work for
	// the coordinator; only a phase change counts there.
	stateProgress := state != work.Run.State && work.Run.EffectPhase != store.BackgroundRunEffectAdmitted
	return phase != work.Run.EffectPhase || stateProgress, nil
}

func (c *Coordinator) runOnce(ctx context.Context) (store.BackgroundRunWork, error) {
	now, err := c.freshNow()
	if err != nil {
		return store.BackgroundRunWork{}, err
	}
	work, err := c.store.NextBackgroundRunWork(ctx, c.config.WorkspaceID, c.config.Profile)
	if errors.Is(err, store.ErrNotFound) {
		return work, ErrNoWork
	}
	if err != nil {
		return work, err
	}
	if work.Run.State == store.BackgroundRunQueued {
		// Consume the provisioning slot before any external effect.
		queued := work.Run
		work.Run, err = c.store.StartBackgroundRunProvisioning(ctx, ref(queued, now))
		if err != nil {
			work.Run = queued
			return work, err
		}
	}
	now, err = c.freshNow()
	if err != nil {
		return work, err
	}
	lifecycle := classify(work.Run)
	if !lifecycle.Valid {
		return work, store.ErrCorruptStore
	}
	if work.Run.StopReceiptID == 0 && work.Run.TimeoutRequestedAt == nil && lifecycle.TimeoutEligible &&
		!now.Before(work.Run.Deadline) {
		return work, c.requestTimeout(ctx, work.Run)
	}
	configurationDiffers := work.Run.ResourceSpecVersion != domain.ResourceSpecVersion || work.Run.ImageIdentity != c.config.ImageIdentity || work.Run.EnvironmentSHA256 != c.config.EnvironmentSHA256 ||
		work.Run.Agent != c.config.Agent || work.Run.ModelProvider != c.config.ModelProvider || work.Run.Model != c.config.Model
	if configurationDiffers && lifecycle.Executing {
		return work, c.cleanupRequired(ctx, work, "configured execution identity differs")
	}
	operation, cancel, _, err := c.effectContext(ctx, work, lifecycle.Executing)
	if err != nil {
		return work, err
	}
	defer cancel()
	return work, c.process(operation, ctx, work)
}

func (c *Coordinator) process(operation, parent context.Context, work store.BackgroundRunWork) error {
	run := work.Run
	switch run.EffectPhase {
	case store.BackgroundRunEffectProvisioning:
		return c.provision(operation, parent, work)
	case store.BackgroundRunEffectPromptPending:
		client, err := c.live(operation, run)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.reconcilePrompt(parent, work, client)
	case store.BackgroundRunEffectAdmitted:
		client, err := c.live(operation, run)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		return c.observeWorking(operation, parent, work, client)
	case store.BackgroundRunEffectSealing:
		return c.seal(operation, parent, work)
	case store.BackgroundRunEffectCleaning:
		return c.clean(operation, parent, work)
	default:
		return nil
	}
}

// seal proves the exact writer stopped, records that fence once, and exports
// under it. The run stays sealing, retaining every resource, until the
// retained result commits.
func (c *Coordinator) seal(operation, parent context.Context, work store.BackgroundRunWork) error {
	if work.Run.WriterFence == nil {
		_, providerFence, err := c.provider.ProveWriterInactive(operation, work.Run)
		if err != nil {
			return fmt.Errorf("prove retained writer inactivity: %w", err)
		}
		if work.Run, err = c.recordWriterFence(parent, work, providerFence); err != nil {
			return fmt.Errorf("record retained writer fence: %w", err)
		}
	}
	return c.exportRetained(operation, parent, work.Run)
}

// clean reconciles teardown in one pass: drain the route, stop the exact
// writer, then remove container, volume, and clone. Each step re-inspects and
// treats absence as done, so a failed pass is simply retried; only when every
// resource is proven absent does the run become terminal.
func (c *Coordinator) clean(operation, parent context.Context, work store.BackgroundRunWork) error {
	run := work.Run
	if run.ObservedContainerID != "" || run.ObservedContainerStartedAt != "" || run.RuntimeEpoch != 0 {
		identity, err := c.validatedRouteIdentity(run)
		if err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		if _, err := c.config.Route.Remove(operation, identity); err != nil {
			return c.cleanupFailure(parent, work, err)
		}
		if err := c.config.Route.ConfirmRemoval(identity); err != nil {
			return c.cleanupFailure(parent, work, err)
		}
	}
	_, authority, err := c.provider.ProveWriterInactive(operation, run)
	if err != nil {
		return c.cleanupFailure(parent, work, err)
	}
	if _, err := c.provider.RemoveContainer(operation, run, authority); err != nil {
		return c.cleanupFailure(parent, work, err)
	}
	if _, err := c.provider.RemoveVolume(operation, run, authority); err != nil {
		return c.cleanupFailure(parent, work, err)
	}
	if _, err := c.provider.RemoveClone(operation, run, authority); err != nil {
		return c.cleanupFailure(parent, work, err)
	}
	return c.terminalize(parent, work)
}

func (c *Coordinator) terminalize(parent context.Context, work store.BackgroundRunWork) error {
	run := work.Run
	if run.Seal != nil {
		mutation, cancel, now, err := c.effectContext(parent, work, false)
		if err != nil {
			return err
		}
		defer cancel()
		_, err = c.store.CompleteBackgroundRunResultCleanup(mutation, store.CompleteBackgroundRunResultCleanupParams{
			BackgroundRunRef: ref(run, now), CleanupProof: `{"route":"absent","container":"absent","volume":"absent","clone":"absent"}`})
		return err
	}
	reason := "runtime_unavailable"
	if run.TimeoutRequestedAt != nil {
		reason = "run_timeout"
	} else if run.StopReceiptID != 0 {
		reason = "user_stopped"
	}
	mutation, cancel, now, err := c.effectContext(parent, work, false)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.FinalizeBackgroundRunFailure(mutation, store.FinalizeBackgroundRunFailureParams{
		BackgroundRunRef: ref(run, now), Reason: reason, Evidence: `{"effect":"terminalize","status":"resources_absent"}`,
		CleanupProof: `{"route":"absent","container":"absent","volume":"absent","clone":"absent"}`,
	})
	return err
}

// recordWriterFence persists the structured provider fence; provider prose
// (the observation evidence) is not durable authority and is not recorded.
func (c *Coordinator) recordWriterFence(ctx context.Context, work store.BackgroundRunWork, provider docker.WriterFence) (store.BackgroundRun, error) {
	run := work.Run
	now, err := c.freshNow()
	if err != nil {
		return run, err
	}
	params := store.RecordBackgroundRunWriterFenceParams{BackgroundRunRef: ref(run, now)}
	switch provider.Kind() {
	case docker.WriterFenceNeverCreated:
		params.Kind = store.WriterFenceNeverCreated
	case docker.WriterFenceCreatedNeverStarted:
		params.Kind, params.ContainerID = store.WriterFenceNeverStarted, provider.ContainerID()
	case docker.WriterFenceStoppedRuntime:
		params.Kind, params.ContainerID, params.ContainerStartedAt = store.WriterFenceRuntimeStopped, provider.ContainerID(), provider.StartedAt()
		params.RuntimeToken, params.StoppedAt = provider.Token(), &now
	default:
		return run, docker.ErrIdentityMismatch
	}
	mutation, cancel, _, err := c.effectContext(ctx, work, false)
	if err != nil {
		return run, err
	}
	defer cancel()
	return c.store.RecordBackgroundRunWriterFence(mutation, params)
}

// exportRetained reconciles the export in one pass. Selection is the only
// durable step before the commit: the snapshot is deterministic from the
// stopped clone, CAS installation is content-addressed, and materialization is
// a verification, so a pass interrupted anywhere re-derives and re-checks them
// against the selected result.
func (c *Coordinator) exportRetained(operation, parent context.Context, run store.BackgroundRun) error {
	attempt := retainedExportAttempt{coordinator: c, run: run}
	selected, err := c.store.GetResult(parent, run.Seal.ResultID)
	switch {
	case err == nil:
		attempt.selected = &selected
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	if err := attempt.reconcile(operation, parent); err != nil {
		return attempt.recoveryRequired(parent, err)
	}
	return nil
}

// retainedExportAttempt owns the sealing run revision this pass read and the
// selected result, if any. SQL remains the authority for both.
type retainedExportAttempt struct {
	coordinator *Coordinator
	run         store.BackgroundRun
	selected    *store.Result
}

func (a *retainedExportAttempt) reconcile(operation, parent context.Context) (resultErr error) {
	c := a.coordinator
	if !a.installed(operation) {
		source, err := c.provider.AcquireExportSource(operation, a.run, providerFence(*a.run.WriterFence))
		if err != nil {
			return err
		}
		// Keep the exclusive clone lease through materialization and commit, not
		// merely through the Git snapshot. No checkout or staged path escapes.
		defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
		if err := a.snapshotAndInstall(operation, parent, source.RepositoryPath()); err != nil {
			return err
		}
	}
	proof, err := a.materialize(operation)
	if err != nil {
		return err
	}
	return a.commitResult(parent, proof)
}

// installed reports whether CAS already holds exactly the selected result, so
// a lost install response needs no new clone lease. It is read-only.
func (a *retainedExportAttempt) installed(operation context.Context) bool {
	if a.selected == nil {
		return false
	}
	locator, err := artifact.ParseLocator(a.selected.CASLocator())
	if err != nil {
		return false
	}
	snapshot, err := a.coordinator.artifact.Inspect(operation, locator)
	return err == nil && snapshotMatches(snapshot, a.run, *a.selected)
}

func (a *retainedExportAttempt) recoveryRequired(parent context.Context, cause error) error {
	c := a.coordinator
	now, err := c.freshNow()
	if err != nil {
		return errors.Join(cause, err)
	}
	// Cancellation must not interrupt recording an ambiguous effect. Detachment
	// does not grant an unbounded write or bypass the store's revision check.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.config.OperationTimeout)
	defer cancel()
	_, markErr := c.store.MarkBackgroundRunExportRecoveryRequired(ctx, ref(a.run, now), "retained artifact export retry required")
	return errors.Join(cause, markErr)
}

// snapshotAndInstall snapshots the fenced clone, selects the result on first
// export (or proves a replay identical to the selection), and installs it.
func (a *retainedExportAttempt) snapshotAndInstall(operation, parent context.Context, repositoryPath string) (resultErr error) {
	c, run := a.coordinator, a.run
	artifactSource, sourceSpecErr := artifact.NewSource(repositoryPath, run.WorkspaceID, run.RunID)
	profileDigest, profileErr := artifact.NewDigest(sha256.Sum256([]byte(run.Profile)))
	environmentDigest, environmentErr := artifact.NewDigest(run.EnvironmentSHA256)
	if sourceSpecErr != nil || profileErr != nil || environmentErr != nil {
		return errors.Join(sourceSpecErr, profileErr, environmentErr)
	}
	snapshot, staged, snapshotErr := c.artifact.Snapshot(operation, artifact.SnapshotSpec{
		Source: artifactSource, RepositoryID: run.RepositoryID, ResultID: run.Seal.ResultID,
		ImageIdentity: run.ImageIdentity, Profile: run.Profile, ProfileSHA256: profileDigest, EnvironmentSHA256: environmentDigest,
		ResourceSpecVersion: artifact.ResourceSpecVersion, OpenCodeSessionID: run.OpenCodeSessionID, OpenCodeMessageID: run.OpenCodeMessageID,
		SnapshotPolicyVersion: artifact.SnapshotPolicyV1, Base: run.BaseOID, EpochSecond: run.Seal.CommitEpochSeconds(),
	})
	if snapshotErr != nil {
		return snapshotErr
	}
	stored := false
	defer func() {
		if !stored {
			resultErr = errors.Join(resultErr, c.artifact.Discard(staged))
		}
	}()
	if a.selected == nil {
		manifestBytes, manifestDigest, manifestErr := c.artifact.StagedManifest(operation, staged)
		if manifestErr != nil {
			return manifestErr
		}
		collectedAt, err := c.freshNow()
		if err != nil {
			return err
		}
		selected, err := c.store.SelectBackgroundRunSnapshot(parent, store.SelectBackgroundRunSnapshotParams{
			BackgroundRunRef: ref(run, collectedAt), ResultCommit: snapshot.Result, TreeOID: snapshot.Tree,
			ChangeCount: len(snapshot.Changes), ChangesSHA256: snapshot.ChangesSHA256.Bytes(), ArtifactManifest: manifestBytes,
			ArtifactManifestSHA256: manifestDigest.Bytes(), BundleSHA256: snapshot.BundleSHA256.Bytes(), BundleBytes: snapshot.BundleBytes,
			CollectedAt: collectedAt,
		})
		if err != nil {
			return err
		}
		a.selected = &selected
	}
	if !snapshotMatches(snapshot, run, *a.selected) {
		return errors.New("retained snapshot differs from durable selection")
	}
	locator, err := c.artifact.Store(operation, staged)
	if err != nil {
		return err
	}
	stored = true
	inspected, inspectErr := c.artifact.Inspect(operation, locator)
	if inspectErr != nil || !snapshotMatches(inspected, run, *a.selected) || locator.String() != a.selected.CASLocator() {
		return errors.Join(inspectErr, errors.New("installed retained artifact differs from durable selection"))
	}
	return nil
}

// materialize proves the installed artifact checks out to the selected commit
// and tree, closing the checkout before the proof is used.
func (a *retainedExportAttempt) materialize(operation context.Context) ([32]byte, error) {
	locator, err := artifact.ParseLocator(a.selected.CASLocator())
	if err != nil {
		return [32]byte{}, err
	}
	checkout, err := a.coordinator.artifact.Materialize(operation, locator)
	if err != nil {
		return [32]byte{}, err
	}
	proof := materializationProof(*a.selected, checkout.Path())
	return proof, checkout.Close()
}

func (a *retainedExportAttempt) commitResult(parent context.Context, proof [32]byte) error {
	c := a.coordinator
	sealedAt, timeErr := c.freshNow()
	if timeErr != nil {
		return timeErr
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.config.OperationTimeout)
	defer cancel()
	_, err := c.store.CommitBackgroundRunRetainedResult(ctx, store.CommitBackgroundRunRetainedResultParams{
		BackgroundRunRef: ref(a.run, sealedAt), MaterializationProof: proof,
	})
	return err
}

func providerFence(value store.WriterFence) docker.WriterFence {
	switch value.Kind {
	case store.WriterFenceNeverCreated:
		return docker.NeverCreatedAuthority()
	case store.WriterFenceNeverStarted:
		return docker.CreatedContainerAuthority(value.ContainerID)
	default:
		return docker.RuntimeCleanupAuthority(docker.RuntimeIdentity{ContainerID: value.ContainerID, StartedAt: value.ContainerStartedAt, Token: value.RuntimeToken})
	}
}

// snapshotMatches binds a snapshot to the sealing run's identities and the
// durable selection.
func snapshotMatches(snapshot artifact.Snapshot, run store.BackgroundRun, selected store.Result) bool {
	return snapshot.RepositoryID == run.RepositoryID && snapshot.WorkspaceID == run.WorkspaceID && snapshot.RunID == run.RunID &&
		snapshot.ResultID == selected.ID && snapshot.OpenCodeSessionID == run.OpenCodeSessionID && snapshot.OpenCodeMessageID == run.OpenCodeMessageID &&
		snapshot.Base == selected.BaseSHA && snapshot.Result == selected.ResultCommit && snapshot.Tree == selected.TreeOID &&
		snapshot.ChangesSHA256.Bytes() == selected.ChangesSHA256 && snapshot.ManifestSHA256.Bytes() == selected.ManifestSHA256 &&
		snapshot.BundleSHA256.Bytes() == selected.BundleSHA256 && snapshot.BundleBytes == selected.BundleBytes
}

func materializationProof(selected store.Result, path string) [32]byte {
	// Path is deliberately reduced to an observation bit; host paths never enter
	// durable evidence. Engine.Materialize already proves detached clean state.
	payload, _ := json.Marshal(struct {
		Schema  string        `json:"schema"`
		Locator string        `json:"locator"`
		Commit  domain.GitOID `json:"commit"`
		Tree    domain.GitOID `json:"tree"`
		Clean   bool          `json:"clean"`
	}{"fern.taskartifact.materialization.v1", selected.CASLocator(), selected.ResultCommit, selected.TreeOID, path != ""})
	return sha256.Sum256(payload)
}

// provision reconciles every inspectable provisioning effect in one pass and
// ends at the prompt fence. Only the started runtime and the fence are durable:
// the clone, volume, container, health, route, and session are derived from
// the run's deterministic identities and re-observed on every pass.
func (c *Coordinator) provision(operation, parent context.Context, work store.BackgroundRunWork) error {
	if work.Run.ObservedContainerID == "" {
		started, err := c.provider.Provision(operation, work.Run)
		if err != nil {
			return c.externalFailure(parent, work, err)
		}
		mutation, cancel, now, err := c.effectContext(parent, work, true)
		if err != nil {
			return err
		}
		work.Run, err = c.store.RecordBackgroundRunRuntime(mutation, store.RecordBackgroundRunRuntimeParams{
			BackgroundRunRef: ref(work.Run, now), ContainerID: started.ContainerID, ContainerStartedAt: started.ContainerStarted,
			RuntimeEpoch: started.RuntimeEpoch, HostPort: started.HostPort, Evidence: started.Evidence,
		})
		cancel()
		if err != nil {
			return err
		}
	}
	client, err := c.live(operation, work.Run)
	if err != nil {
		return c.externalFailure(parent, work, err)
	}
	ready, err := c.reconcileSession(operation, parent, work, client)
	if err != nil || !ready {
		return err
	}
	return c.dispatchWhenReady(operation, parent, work, client)
}

// dispatchWhenReady crosses the prompt fence only once the session's location
// lists the configured model and agent. OpenCode loads them asynchronously
// after it first opens the location, and a turn started earlier fails without
// durable evidence, stranding an admitted prompt. An unready catalog leaves the
// run provisioning; a later pass retries until the attempt deadline.
func (c *Coordinator) dispatchWhenReady(operation, parent context.Context, work store.BackgroundRunWork, client *opencode.Client) error {
	spec := opencode.ReadinessSpec{Agent: c.config.Agent, ProviderID: c.config.ModelProvider,
		ModelID: c.config.Model, Directory: sessionDirectory}
	if err := client.WaitReady(operation, spec, c.readinessInterval()); err != nil {
		if err := parent.Err(); err != nil {
			return err
		}
		return c.externalFailure(parent, work, err)
	}
	return c.dispatchPrompt(operation, parent, work, client)
}

func (c *Coordinator) readinessInterval() time.Duration {
	return min(max(c.config.PollInterval, readinessMinInterval), readinessMaxInterval)
}

// live proves the committed runtime healthy, refreshes its GitHub credentials,
// and keeps its route active. Credentials are runtime inputs, never
// publication authority, and are refreshed only while execution is allowed.
func (c *Coordinator) live(ctx context.Context, run store.BackgroundRun) (*opencode.Client, error) {
	runtime, err := c.provider.CommittedRuntime(run)
	if err != nil {
		return nil, errors.Join(docker.ErrIdentityMismatch, err)
	}
	if _, err := c.provider.Health(ctx, run, runtime); err != nil {
		return nil, err
	}
	if err := c.provider.RefreshGitHubCredentials(ctx, run); err != nil {
		return nil, err
	}
	target, err := c.provider.BackgroundRouteTarget(run, runtime)
	if err != nil {
		return nil, err
	}
	if _, err := c.config.Route.Activate(makeRouteIdentity(run, runtime), target); err != nil {
		return nil, err
	}
	return c.provider.OpenCodeClient(run, runtime, c.config.HTTPClient)
}

// reconcileSession creates the Fern-chosen session at most once per pass and
// reports whether it exactly exists.
func (c *Coordinator) reconcileSession(operation, parent context.Context, work store.BackgroundRunWork, client *opencode.Client) (bool, error) {
	run := work.Run
	spec := opencode.SessionSpec{ID: string(run.OpenCodeSessionID), Agent: c.config.Agent,
		ProviderID: c.config.ModelProvider, ModelID: c.config.Model, Directory: sessionDirectory}
	state, err := client.ReconcileSession(operation, spec)
	if err == nil && state == opencode.ReconcileAbsent {
		createErr := client.CreateSessionOnce(operation, spec)
		state, err = client.ReconcileSession(operation, spec)
		if err == nil && state == opencode.ReconcileAbsent {
			if createErr == nil {
				return false, c.cleanupRequired(parent, work, "OpenCode session disappeared after creation")
			}
			return false, errors.Join(createErr, c.cleanupRequired(parent, work, "OpenCode session creation is inconclusive"))
		}
	}
	if err != nil {
		return false, c.externalFailure(parent, work, err)
	}
	if state == opencode.ReconcileConflict {
		return false, c.cleanupRequired(parent, work, "OpenCode session identity conflict")
	}
	return state == opencode.ReconcileExact, nil
}

// dispatchPrompt commits the one-way prompt fence, which ends provisioning,
// before the single admission call. Whatever happens next, the run is
// prompt_pending and later passes only reconcile.
func (c *Coordinator) dispatchPrompt(operation, parent context.Context, work store.BackgroundRunWork, client *opencode.Client) error {
	mutation, cancel, now, err := c.effectContext(parent, work, true)
	if err != nil {
		return err
	}
	defer cancel()
	if !now.Before(work.Run.Deadline) {
		return c.requestTimeout(parent, work.Run)
	}
	work.Run, err = c.store.RecordBackgroundRunPromptRequestAttempted(mutation, ref(work.Run, now))
	if err != nil {
		return err
	}
	if c.config.AfterPromptFence != nil {
		c.config.AfterPromptFence()
	}
	if err := operation.Err(); err != nil {
		return err
	}
	if err := c.promptDispatchAuthority(work); errors.Is(err, context.DeadlineExceeded) {
		return c.requestTimeout(parent, work.Run)
	} else if err != nil {
		return err
	}
	callErr := client.AdmitPromptOnce(operation, string(work.Run.OpenCodeSessionID), c.promptSpec(work))
	if c.config.AfterPromptCall != nil {
		c.config.AfterPromptCall(callErr)
	}
	if err := parent.Err(); err != nil {
		return err
	}
	return c.reconcilePrompt(parent, work, client)
}

func (c *Coordinator) promptSpec(work store.BackgroundRunWork) opencode.PromptSpec {
	return opencode.PromptSpec{ID: string(work.Run.OpenCodeMessageID), Text: work.Prompt, Resume: true, Delivery: "steer"}
}

// reconcilePrompt reads bounded session history; only exact admission
// advances, and anything else leaves the prompt uncertain.
func (c *Coordinator) reconcilePrompt(parent context.Context, work store.BackgroundRunWork, client *opencode.Client) error {
	reconcileCtx, reconcileCancel, _, contextErr := c.effectContext(parent, work, true)
	if contextErr != nil {
		return contextErr
	}
	defer reconcileCancel()
	state, reconcileErr := client.ReconcilePrompt(reconcileCtx, string(work.Run.OpenCodeSessionID), c.promptSpec(work), c.config.HistoryBounds)
	if reconcileErr == nil && state == opencode.ReconcileExact {
		return c.record(parent, work, `{"effect":"prompt_reconcile","status":"admitted"}`, c.store.RecordBackgroundRunPromptAdmitted)
	}
	status := "inconclusive"
	if reconcileErr == nil {
		status = string(state)
	}
	return c.record(parent, work, fmt.Sprintf(`{"effect":"prompt_reconcile","status":%q}`, status), c.store.RecordBackgroundRunPromptUncertain)
}

func (c *Coordinator) observeWorking(operation, parent context.Context, work store.BackgroundRunWork, client *opencode.Client) error {
	run := work.Run
	usage, err := c.provider.ObserveUsage(operation, run)
	if err != nil {
		if errors.Is(err, docker.ErrIdentityMismatch) || errors.Is(err, docker.ErrQuarantined) {
			return errors.Join(err, c.cleanupRequired(parent, work, "background usage limit or identity mismatch"))
		}
		return c.externalFailure(parent, work, err)
	}
	observation, err := client.ObservePending(operation, string(run.OpenCodeSessionID))
	if err != nil {
		recordErr := c.recordObservation(parent, work, `{"effect":"work_observe","status":"inconclusive"}`, store.BackgroundRunUncertain)
		return errors.Join(err, recordErr)
	}
	state, status := workObservation(observation.State)
	if state == "" {
		return nil
	}
	value := fmt.Sprintf(`{"effect":"work_observe","status":%q,"questions":%d,"permissions":%d,"usage":%s}`,
		status, observation.Questions, observation.Permissions, usage.Evidence)
	return c.recordObservation(parent, work, value, state)
}

// The runtime owns pending-observation precedence. Counts remain evidence only;
// unknown observations do not authorize a durable state change.
func workObservation(state opencode.WorkState) (store.BackgroundRunState, string) {
	switch state {
	case opencode.WorkNeedsYou:
		return store.BackgroundRunNeedsYou, "owned_pending"
	case opencode.WorkWorking:
		return store.BackgroundRunWorking, "positive_active"
	default:
		return "", ""
	}
}

func (c *Coordinator) validatedRouteIdentity(run store.BackgroundRun) (opencode.RouteIdentity, error) {
	runtime, err := c.provider.CommittedRuntime(run)
	if err != nil {
		return opencode.RouteIdentity{}, err
	}
	return makeRouteIdentity(run, runtime), nil
}

func makeRouteIdentity(run store.BackgroundRun, runtime docker.RuntimeIdentity) opencode.RouteIdentity {
	return opencode.RouteIdentity{WorkspaceID: string(run.WorkspaceID), RunID: string(run.RunID),
		SessionID: string(run.OpenCodeSessionID), RuntimeEpoch: run.RuntimeEpoch,
		ContainerID: runtime.ContainerID, StartedAt: runtime.StartedAt, RuntimeToken: runtime.Token}
}

func (c *Coordinator) externalFailure(ctx context.Context, work store.BackgroundRunWork, external error) error {
	if errors.Is(external, docker.ErrIdentityMismatch) || errors.Is(external, docker.ErrQuarantined) {
		return errors.Join(external, c.cleanupRequired(ctx, work, "background resource identity mismatch"))
	}
	if errors.Is(external, docker.ErrRuntimeExited) {
		return errors.Join(external, c.cleanupRequired(ctx, work, "background container exited before its runtime was recorded"))
	}
	return external
}

func (c *Coordinator) cleanupRequired(ctx context.Context, work store.BackgroundRunWork, reason string) error {
	if identity, identityErr := c.validatedRouteIdentity(work.Run); identityErr == nil && c.config.Route.Active(identity) {
		// Remove waits for in-flight proxied requests to drain; bound it like
		// every other effect so a stuck request cannot hold the scan.
		removal, cancel, _, err := c.effectContext(ctx, work, false)
		if err != nil {
			return err
		}
		_, removeErr := c.config.Route.Remove(removal, identity)
		cancel()
		if removeErr != nil {
			return removeErr
		}
	}
	mutation, cancel, now, err := c.effectContext(ctx, work, classify(work.Run).Executing)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.MarkBackgroundRunCleanupRequired(mutation, store.MarkBackgroundRunCleanupRequiredParams{
		BackgroundRunRef: ref(work.Run, now), Error: reason,
	})
	return err
}

func (c *Coordinator) cleanupFailure(ctx context.Context, work store.BackgroundRunWork, external error) error {
	return errors.Join(external, c.cleanupRequired(ctx, work, "background cleanup retry required"))
}

func (c *Coordinator) freshNow() (time.Time, error) {
	raw := c.config.Now()
	if raw.IsZero() || raw.UnixMilli() < 0 {
		return time.Time{}, errors.New("background run clock returned an invalid timestamp")
	}
	return raw.UTC().Truncate(time.Millisecond), nil
}

func (c *Coordinator) promptDispatchAuthority(work store.BackgroundRunWork) error {
	now, err := c.freshNow()
	if err != nil {
		return err
	}
	if !now.Before(work.Run.Deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *Coordinator) effectContext(parent context.Context, work store.BackgroundRunWork, enforceRunDeadline bool) (context.Context, context.CancelFunc, time.Time, error) {
	now, err := c.freshNow()
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	deadline := now.Add(c.config.OperationTimeout)
	if enforceRunDeadline && work.Run.Deadline.Before(deadline) {
		deadline = work.Run.Deadline
	}
	if !deadline.After(now) {
		return nil, nil, now, context.DeadlineExceeded
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	return ctx, cancel, now, nil
}

func (c *Coordinator) requestTimeout(ctx context.Context, run store.BackgroundRun) error {
	work := store.BackgroundRunWork{Run: run}
	mutation, cancel, now, err := c.effectContext(ctx, work, false)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.RequestBackgroundRunTimeout(mutation, ref(run, now))
	return err
}

func (c *Coordinator) record(ctx context.Context, work store.BackgroundRunWork, value string,
	transition func(context.Context, store.RecordBackgroundRunEvidenceParams) (store.BackgroundRun, error)) error {
	mutation, cancel, now, err := c.effectContext(ctx, work, classify(work.Run).Executing)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = transition(mutation, evidence(work.Run, now, value))
	return err
}

func (c *Coordinator) recordObservation(ctx context.Context, work store.BackgroundRunWork, value string, state store.BackgroundRunState) error {
	mutation, cancel, now, err := c.effectContext(ctx, work, true)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.store.RecordBackgroundRunWorkObservation(mutation, evidence(work.Run, now, value), state)
	return err
}

// ref pins the revision this scan read; every write is a compare-and-swap on it.
func ref(run store.BackgroundRun, now time.Time) store.BackgroundRunRef {
	return store.BackgroundRunRef{WorkspaceID: run.WorkspaceID, RunID: run.RunID,
		ExpectedRevision: run.Revision, ExpectedState: run.State, ExpectedPhase: run.EffectPhase, Now: now}
}

func evidence(run store.BackgroundRun, now time.Time, value string) store.RecordBackgroundRunEvidenceParams {
	return store.RecordBackgroundRunEvidenceParams{BackgroundRunRef: ref(run, now), Evidence: value}
}

func classify(run store.BackgroundRun) domain.Lifecycle {
	return domain.Classify(domain.State(run.State), domain.Phase(run.EffectPhase))
}
