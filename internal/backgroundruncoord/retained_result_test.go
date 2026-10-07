package backgroundruncoord

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nebler/fern/internal/artifact"
	"github.com/nebler/fern/internal/docker"
	"github.com/nebler/fern/internal/opencode"
	"github.com/nebler/fern/internal/store"
)

func TestExportRecoveryKeepsOriginalFailure(t *testing.T) {
	a := retainedExportAttempt{coordinator: &Coordinator{config: Config{Now: func() time.Time { return time.Time{} }}}}
	failure := errors.New("SQL response failed")
	if err := a.recoveryRequired(context.Background(), failure); !errors.Is(err, failure) {
		t.Fatalf("recovery clock error hid original failure: %v", err)
	}
}

func retainedTuple(t *testing.T) (artifact.Snapshot, store.BackgroundRun, store.Result) {
	t.Helper()
	digest, err := artifact.NewDigest([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := artifact.Snapshot{RepositoryID: 42, WorkspaceID: "workspace", RunID: "task", ResultID: "result",
		OpenCodeSessionID: "session", OpenCodeMessageID: "message", Base: "base", Result: "result", Tree: "tree",
		ChangesSHA256: digest, ManifestSHA256: digest, BundleSHA256: digest, BundleBytes: 42}
	run := store.BackgroundRun{RepositoryID: snapshot.RepositoryID, WorkspaceID: snapshot.WorkspaceID, RunID: snapshot.RunID,
		OpenCodeSessionID: snapshot.OpenCodeSessionID, OpenCodeMessageID: snapshot.OpenCodeMessageID}
	result := store.Result{ID: snapshot.ResultID, RunID: snapshot.RunID, BaseSHA: snapshot.Base, ResultCommit: snapshot.Result,
		TreeOID: snapshot.Tree, ChangesSHA256: digest.Bytes(), ManifestSHA256: digest.Bytes(), BundleSHA256: digest.Bytes(), BundleBytes: snapshot.BundleBytes}
	return snapshot, run, result
}

func TestSnapshotMatchesEveryDurableSelectionField(t *testing.T) {
	snapshot, run, result := retainedTuple(t)
	if !snapshotMatches(snapshot, run, result) {
		t.Fatal("exact tuple rejected")
	}
	for _, field := range []string{"RepositoryID", "WorkspaceID", "RunID", "ResultID", "OpenCodeSessionID", "OpenCodeMessageID", "Base", "Result", "Tree", "ChangesSHA256", "ManifestSHA256", "BundleSHA256", "BundleBytes"} {
		t.Run(field, func(t *testing.T) {
			changed := snapshot
			reflect.ValueOf(&changed).Elem().FieldByName(field).SetZero()
			if snapshotMatches(changed, run, result) {
				t.Fatal("mismatched tuple accepted")
			}
		})
	}
}

func TestMaterializationProofOmitsHostPathButBindsIdentity(t *testing.T) {
	_, _, result := retainedTuple(t)
	proof := materializationProof(result, "/first/private/checkout")
	if proof != materializationProof(result, "/different/host/path") || proof == materializationProof(result, "") {
		t.Fatal("proof must retain observation bit, not host path")
	}
	for _, field := range []string{"ManifestSHA256", "ResultCommit", "TreeOID"} {
		changed := result
		reflect.ValueOf(&changed).Elem().FieldByName(field).SetZero()
		if proof == materializationProof(changed, "/checkout") {
			t.Fatalf("proof does not bind %s", field)
		}
	}
}

// Embedding the unused boundary makes any unexpected artifact mutation panic.
type failingRetainedArtifact struct {
	Artifact
	snapshot artifact.Snapshot
	err      error
}

func (f failingRetainedArtifact) Inspect(context.Context, artifact.Locator) (artifact.Snapshot, error) {
	return f.snapshot, f.err
}

func (f failingRetainedArtifact) Materialize(context.Context, artifact.Locator) (*artifact.Checkout, error) {
	return nil, f.err
}

// Only an exact CAS inspection of the selected tuple skips the fenced
// snapshot replay; anything else re-derives from the stopped clone.
func TestRetainedInstallationSkipsSnapshotOnlyForExactSelection(t *testing.T) {
	snapshot, run, result := retainedTuple(t)
	failure := errors.New("artifact unavailable")
	for _, tt := range []struct {
		name     string
		selected *store.Result
		artifact failingRetainedArtifact
		want     bool
	}{
		{"unselected", nil, failingRetainedArtifact{snapshot: snapshot}, false},
		{"inspection failure", &result, failingRetainedArtifact{err: failure}, false},
		{"mismatched", &result, failingRetainedArtifact{}, false},
		{"exact", &result, failingRetainedArtifact{snapshot: snapshot}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := retainedExportAttempt{coordinator: &Coordinator{artifact: tt.artifact}, run: run, selected: tt.selected}
			if got := a.installed(context.Background()); got != tt.want {
				t.Fatalf("installed = %v, want %v", got, tt.want)
			}
		})
	}
	a := retainedExportAttempt{coordinator: &Coordinator{artifact: failingRetainedArtifact{err: failure}}, run: run, selected: &result}
	if _, err := a.materialize(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("materialization failure = %v", err)
	}
}

func TestMakeRouteIdentityProjectsCommittedRuntime(t *testing.T) {
	run := store.BackgroundRun{WorkspaceID: "workspace", RunID: "task", RuntimeEpoch: 4, OpenCodeSessionID: "session",
		ObservedContainerID: "not-the-attested-runtime"}
	runtime := docker.RuntimeIdentity{ContainerID: "container", StartedAt: "started", Token: "token"}
	want := opencode.RouteIdentity{WorkspaceID: "workspace", RunID: "task", RuntimeEpoch: 4, SessionID: "session", ContainerID: "container", StartedAt: "started", RuntimeToken: "token"}
	if got := makeRouteIdentity(run, runtime); got != want {
		t.Fatalf("route tuple = %+v, want %+v", got, want)
	}
}
