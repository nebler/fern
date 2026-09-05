package backgroundruncoord

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

func TestExportTransitionKeepsLastConfirmedClaim(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 123456789, time.UTC)
	a := retainedExportAttempt{coordinator: &Coordinator{config: Config{Now: func() time.Time { return now }}}, export: taskstore.BackgroundRunExport{
		ID: "export", TaskID: "task", AttemptID: "attempt", Generation: 3, Revision: 7,
		Phase: taskstore.BackgroundRunExportPhaseSnapshotSelected, ClaimOwner: "worker", ClaimGeneration: 5,
	}}
	want := taskstore.BackgroundRunExportClaim{ExportID: "export", TaskID: "task", AttemptID: "attempt", Generation: 3,
		ExpectedRevision: 7, ExpectedPhase: taskstore.BackgroundRunExportPhaseSnapshotSelected,
		ClaimOwner: "worker", ClaimGeneration: 5, Now: now.Truncate(time.Millisecond)}
	failure := errors.New("SQL response failed")
	err := a.record(func(got taskstore.BackgroundRunExportClaim) (taskstore.BackgroundRunExport, error) {
		if got != want {
			t.Fatalf("claim = %+v, want %+v", got, want)
		}
		return taskstore.BackgroundRunExport{}, failure
	})
	if !errors.Is(err, failure) || exportClaim(a.export, want.Now) != want {
		t.Fatalf("failed transition lost authority: %+v, %v", a.export, err)
	}
	now = now.Add(time.Second)
	err = a.record(func(got taskstore.BackgroundRunExportClaim) (taskstore.BackgroundRunExport, error) {
		if !got.Now.Equal(now.Truncate(time.Millisecond)) {
			t.Fatal("transition reused old clock")
		}
		next := a.export
		next.Revision++
		next.Phase = taskstore.BackgroundRunExportPhaseBundleWriteStarted
		return next, nil
	})
	if err != nil || a.export.Revision != 8 || a.export.Phase != taskstore.BackgroundRunExportPhaseBundleWriteStarted {
		t.Fatalf("successful transition not adopted: %+v, %v", a.export, err)
	}
	now = time.Time{}
	err = a.record(func(taskstore.BackgroundRunExportClaim) (taskstore.BackgroundRunExport, error) {
		t.Fatal("invalid clock reached SQL")
		return taskstore.BackgroundRunExport{}, nil
	})
	if err == nil || a.export.Revision != 8 {
		t.Fatalf("invalid clock changed authority: %+v, %v", a.export, err)
	}
	if err := a.recoveryRequired(context.Background(), failure); !errors.Is(err, failure) {
		t.Fatalf("recovery clock error hid original failure: %v", err)
	}
}

func retainedTuple(t *testing.T) (taskartifact.Snapshot, taskstore.BackgroundRunExport) {
	t.Helper()
	digest, err := taskartifact.NewDigest([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := taskartifact.Snapshot{RepositoryID: 42, WorkspaceID: "workspace", TaskID: "task", AttemptID: "attempt",
		Generation: 1, SealRequestID: "seal", Base: "base", Result: "result", Tree: "tree",
		ChangesSHA256: digest, ManifestSHA256: digest, BundleSHA256: digest, BundleBytes: 42}
	export := taskstore.BackgroundRunExport{RepositoryID: snapshot.RepositoryID, WorkspaceID: snapshot.WorkspaceID,
		TaskID: snapshot.TaskID, AttemptID: snapshot.AttemptID, Generation: snapshot.Generation, SealRequestID: snapshot.SealRequestID,
		BaseSHA: snapshot.Base, ResultCommit: snapshot.Result, TreeOID: snapshot.Tree, ChangesSHA256: digest.Bytes(),
		ArtifactManifestSHA256: digest.Bytes(), BundleSHA256: digest.Bytes(), BundleBytes: snapshot.BundleBytes, CASLocator: "sha256:" + digest.String()}
	return snapshot, export
}

func TestSnapshotMatchesEveryDurableSelectionField(t *testing.T) {
	snapshot, export := retainedTuple(t)
	if !snapshotMatchesExport(snapshot, export) {
		t.Fatal("exact tuple rejected")
	}
	for _, field := range []string{"RepositoryID", "WorkspaceID", "TaskID", "AttemptID", "Generation", "SealRequestID", "Base", "Result", "Tree", "ChangesSHA256", "ManifestSHA256", "BundleSHA256", "BundleBytes"} {
		t.Run(field, func(t *testing.T) {
			changed := snapshot
			reflect.ValueOf(&changed).Elem().FieldByName(field).SetZero()
			if snapshotMatchesExport(changed, export) {
				t.Fatal("mismatched tuple accepted")
			}
		})
	}
	export.BundleSHA256 = [32]byte{}
	export.BundleBytes++
	if !snapshotMatchesExport(snapshot, export) {
		t.Fatal("selection before bundle verification incorrectly requires bundle evidence")
	}
}

func TestRetainedSnapshotReplayStopsAfterCASInstallation(t *testing.T) {
	for _, tt := range []struct {
		phase taskstore.BackgroundRunExportPhase
		want  bool
	}{
		{taskstore.BackgroundRunExportPhasePrepared, true},
		{taskstore.BackgroundRunExportPhaseSnapshotStarted, true},
		{taskstore.BackgroundRunExportPhaseSnapshotSelected, true},
		{taskstore.BackgroundRunExportPhaseBundleWriteStarted, true},
		{taskstore.BackgroundRunExportPhaseBundleVerified, true},
		{taskstore.BackgroundRunExportPhaseCASInstallStarted, true},
		{taskstore.BackgroundRunExportPhaseCASInstalled, false},
		{taskstore.BackgroundRunExportPhaseMaterializeStarted, false},
		{taskstore.BackgroundRunExportPhaseMaterialized, false},
		{taskstore.BackgroundRunExportPhaseCompleted, false},
		{"unknown", false},
	} {
		if got := needsRetainedSnapshot(tt.phase); got != tt.want {
			t.Errorf("snapshot replay for %q = %v, want %v", tt.phase, got, tt.want)
		}
	}
}

func TestManifestProjectionOwnsOptionalFileVersions(t *testing.T) {
	changes := []taskartifact.ChangeEntry{
		{PathBase64: "/w==", Kind: "added", New: &taskartifact.FileVersion{Mode: "100644", BlobOID: "new", Size: 0}},
		{PathBase64: "Yg==", Kind: "deleted", Old: &taskartifact.FileVersion{Mode: "100755", BlobOID: "old", Size: 2}},
	}
	got := manifestEntries(changes)
	if got[0].PathBase64 != "/w==" || got[0].ChangeKind != "added" || got[0].OldMode != nil || got[0].NewSize == nil || *got[0].NewSize != 0 ||
		got[1].NewMode != nil || *got[1].OldMode != "100755" || *got[1].OldBlobOID != "old" || *got[1].OldSize != 2 {
		t.Fatalf("projection lost raw path or optional metadata: %+v", got)
	}
	changes[0].New.Mode = "changed"
	changes[1].Old.Size = 99
	if *got[0].NewMode != "100644" || *got[1].OldSize != 2 {
		t.Fatal("projection aliases input metadata")
	}
}

func TestMaterializationProofOmitsHostPathButBindsIdentity(t *testing.T) {
	_, export := retainedTuple(t)
	proof := materializationProof(export, "/first/private/checkout")
	if proof != materializationProof(export, "/different/host/path") || proof == materializationProof(export, "") {
		t.Fatal("proof must retain observation bit, not host path")
	}
	for _, field := range []string{"CASLocator", "ResultCommit", "TreeOID"} {
		changed := export
		reflect.ValueOf(&changed).Elem().FieldByName(field).SetZero()
		if proof == materializationProof(changed, "/checkout") {
			t.Fatalf("proof does not bind %s", field)
		}
	}
}

// Embedding the unused boundary makes any unexpected artifact mutation panic.
type failingRetainedArtifact struct {
	Artifact
	snapshot taskartifact.Snapshot
	err      error
}

func (f failingRetainedArtifact) Inspect(context.Context, taskartifact.Locator) (taskartifact.Snapshot, error) {
	return f.snapshot, f.err
}

func (f failingRetainedArtifact) Materialize(context.Context, taskartifact.Locator) (*taskartifact.Checkout, error) {
	return nil, f.err
}

func TestRetainedRecoveryAndMaterializationFailuresDoNotAdvance(t *testing.T) {
	snapshot, export := retainedTuple(t)
	export.Phase = taskstore.BackgroundRunExportPhaseCASInstallStarted
	failure := errors.New("artifact unavailable")
	a := retainedExportAttempt{coordinator: &Coordinator{artifact: failingRetainedArtifact{err: failure}}, export: export}
	if err := a.recoverInstalledCAS(context.Background(), context.Background()); err != nil || !needsRetainedSnapshot(a.export.Phase) {
		t.Fatalf("failed CAS inspection must leave fenced snapshot replay available: %v", err)
	}
	a.coordinator.artifact = failingRetainedArtifact{snapshot: taskartifact.Snapshot{}}
	if err := a.recoverInstalledCAS(context.Background(), context.Background()); err != nil || a.export.Phase != export.Phase {
		t.Fatalf("mismatched CAS advanced export: %v", err)
	}
	// Exact inspection may record recovery, but an invalid clock must prevent SQL.
	a.coordinator.config.Now = func() time.Time { return time.Time{} }
	a.coordinator.artifact = failingRetainedArtifact{snapshot: snapshot}
	if err := a.recoverInstalledCAS(context.Background(), context.Background()); err == nil || a.export.Phase != export.Phase {
		t.Fatalf("invalid clock permitted CAS recovery: %v", err)
	}
	a.export.Phase = taskstore.BackgroundRunExportPhaseMaterializeStarted
	a.coordinator.artifact = failingRetainedArtifact{err: failure}
	if err := a.verifyMaterialization(context.Background(), context.Background()); !errors.Is(err, failure) || a.export.Phase != taskstore.BackgroundRunExportPhaseMaterializeStarted {
		t.Fatalf("materialization failure advanced export: %v", err)
	}
}

func TestMakeRouteIdentityProjectsCommittedRuntime(t *testing.T) {
	run := taskstore.BackgroundRun{WorkspaceID: "workspace", TaskID: "task", AttemptID: "attempt", Generation: 2,
		WriterGeneration: 3, RuntimeEpoch: 4, OpenCodeSessionID: "session", ObservedContainerID: "not-the-attested-runtime"}
	runtime := taskenvdocker.RuntimeIdentity{ContainerID: "container", StartedAt: "started", Token: "token"}
	want := backgroundroute.Identity{WorkspaceID: "workspace", TaskID: "task", AttemptID: "attempt", Generation: 2,
		WriterGeneration: 3, RuntimeEpoch: 4, SessionID: "session", ContainerID: "container", StartedAt: "started", RuntimeToken: "token"}
	if got := makeRouteIdentity(run, runtime); got != want {
		t.Fatalf("route tuple = %+v, want %+v", got, want)
	}
}
