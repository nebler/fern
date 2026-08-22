package publication

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/nebler/fern/internal/control"
)

var (
	ErrRunning = errors.New("publication is already executing")
	ErrClosed  = errors.New("publication coordinator is closed")
)

type Store interface {
	Publication(string) (control.Publication, bool)
	Publications() []control.Publication
	PreparePublication(id string, prepared control.PreparedPublication, now time.Time) error
	FinishPublication(id string, pullRequest *control.PullRequestObservation, failure string, now time.Time) (control.Publication, error)
}

type Fencer interface {
	AcquirePaused(context.Context) (func(), error)
}

type Backend interface {
	Prepare(context.Context, Request) (Prepared, error)
	PublishPrepared(context.Context, Prepared, string, string) (Result, error)
}

type execution struct {
	done   chan struct{}
	record control.Publication
	err    error
}

// Coordinator owns all publication workers and guarantees that an immutable
// publication target is durable before any GitHub mutation occurs.
type Coordinator struct {
	store   Store
	fencer  Fencer
	backend Backend
	timeout time.Duration

	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	running map[string]*execution
	closed  bool
	wg      sync.WaitGroup
}

func NewCoordinator(parent context.Context, store Store, fencer Fencer, backend Backend) *Coordinator {
	ctx, cancel := context.WithCancel(parent)
	return &Coordinator{
		store: store, fencer: fencer, backend: backend, timeout: 2 * time.Minute,
		ctx: ctx, cancel: cancel, running: make(map[string]*execution),
	}
}

// Execute starts one service-owned operation and waits for it. Canceling the
// caller only stops waiting; it does not abandon the durable external effect.
func (coordinator *Coordinator) Execute(ctx context.Context, id string) (control.Publication, error) {
	execution, err := coordinator.start(id)
	if err != nil {
		record, _ := coordinator.store.Publication(id)
		return record, err
	}
	select {
	case <-ctx.Done():
		return control.Publication{}, ctx.Err()
	case <-execution.done:
		return execution.record, execution.err
	}
}

func (coordinator *Coordinator) start(id string) (*execution, error) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.closed {
		return nil, ErrClosed
	}
	if _, exists := coordinator.running[id]; exists {
		return nil, ErrRunning
	}
	execution := &execution{done: make(chan struct{})}
	coordinator.running[id] = execution
	coordinator.wg.Add(1)
	go coordinator.run(id, execution)
	return execution, nil
}

func (coordinator *Coordinator) run(id string, execution *execution) {
	defer coordinator.wg.Done()
	ctx, cancel := context.WithTimeout(coordinator.ctx, coordinator.timeout)
	execution.record, execution.err = coordinator.execute(ctx, id)
	cancel()
	coordinator.mu.Lock()
	delete(coordinator.running, id)
	close(execution.done)
	coordinator.mu.Unlock()
}

func (coordinator *Coordinator) execute(ctx context.Context, id string) (control.Publication, error) {
	record, exists := coordinator.store.Publication(id)
	if !exists {
		return control.Publication{}, os.ErrNotExist
	}
	if record.State == control.PublicationPublished {
		return record, nil
	}
	if record.SchemaVersion != control.PublicationSchemaVersion {
		return record, errors.New("legacy publication recovery is blocked and requires operator review")
	}
	release, err := coordinator.fencer.AcquirePaused(ctx)
	if err != nil {
		return record, err
	}
	defer release()
	record, exists = coordinator.store.Publication(id)
	if !exists {
		return control.Publication{}, os.ErrNotExist
	}
	if record.State == control.PublicationPublished {
		return record, nil
	}
	if record.SchemaVersion != control.PublicationSchemaVersion {
		return record, errors.New("legacy publication recovery is blocked and requires operator review")
	}

	prepared := preparedFromControl(record)
	if record.ResultCommit == "" {
		prepared, err = coordinator.backend.Prepare(ctx, Request{
			Operation: record.Operation, Base: record.RequestedBaseRef, Title: record.Title, Body: record.Body,
		})
		if err == nil {
			err = coordinator.store.PreparePublication(record.ID, preparedToControl(prepared), time.Now())
		}
		if err == nil {
			record, exists = coordinator.store.Publication(id)
			if !exists {
				return control.Publication{}, os.ErrNotExist
			}
			prepared = preparedFromControl(record)
		}
	}
	if err == nil {
		var result Result
		result, err = coordinator.backend.PublishPrepared(ctx, prepared, record.Title, record.Body)
		if err == nil && result.Prepared != prepared {
			err = errors.New("publication backend returned a different prepared target")
		}
		if err == nil {
			observation := pullRequestToControl(result.PullRequest)
			return coordinator.store.FinishPublication(record.ID, &observation, "", time.Now())
		}
	}
	if ctx.Err() == nil {
		if _, finishErr := coordinator.store.FinishPublication(record.ID, nil, "publication failed", time.Now()); finishErr != nil {
			return record, errors.Join(err, finishErr)
		}
	}
	return record, err
}

// Reconcile schedules interrupted operations through the same tracked worker
// path used by live requests.
func (coordinator *Coordinator) Reconcile() error {
	for _, record := range coordinator.store.Publications() {
		if record.State != control.PublicationRequested && record.State != control.PublicationPrepared {
			continue
		}
		if record.SchemaVersion != control.PublicationSchemaVersion {
			continue
		}
		if _, err := coordinator.start(record.ID); err != nil && !errors.Is(err, ErrRunning) {
			return err
		}
	}
	return nil
}

func preparedToControl(prepared Prepared) control.PreparedPublication {
	return control.PreparedPublication{
		RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName,
		BaseSHA: prepared.BaseSHA, BaseRef: prepared.BaseRef,
		ResultCommit: prepared.ResultCommit, Branch: prepared.Branch,
	}
}

func preparedFromControl(record control.Publication) Prepared {
	return Prepared{
		RepositoryID: record.RepositoryID, RepositoryFullName: record.RepositoryFullName,
		BaseSHA: record.BaseSHA, BaseRef: record.BaseRef,
		ResultCommit: record.ResultCommit, Branch: record.Branch,
	}
}

func pullRequestToControl(observation PullRequestObservation) control.PullRequestObservation {
	return control.PullRequestObservation{
		TargetRepositoryID: observation.TargetRepositoryID, TargetRepositoryFullName: observation.TargetRepositoryFullName,
		Number: observation.Number, URL: observation.URL, State: observation.State, Draft: observation.Draft,
		Base: pullRequestRefToControl(observation.Base), Head: pullRequestRefToControl(observation.Head),
	}
}

func pullRequestRefToControl(observation PullRequestRefObservation) control.PullRequestRefObservation {
	return control.PullRequestRefObservation{
		RepositoryID: observation.RepositoryID, RepositoryFullName: observation.RepositoryFullName,
		RepositoryOwner: observation.RepositoryOwner, RepositoryName: observation.RepositoryName,
		Ref: observation.Ref, SHA: observation.SHA,
	}
}

func (coordinator *Coordinator) Close(ctx context.Context) error {
	coordinator.mu.Lock()
	if !coordinator.closed {
		coordinator.closed = true
		coordinator.cancel()
	}
	coordinator.mu.Unlock()
	done := make(chan struct{})
	go func() {
		coordinator.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
