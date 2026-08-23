package taskresultcoord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

type fakeAuthorizationStore struct {
	preview      taskstore.SealPreview
	previewCalls int
	requestCalls int
	fencer       *fakeFencer
	params       taskstore.RequestSealParams
}

func (store *fakeAuthorizationStore) GetSealPreview(context.Context, task.TaskID) (taskstore.SealPreview, error) {
	store.previewCalls++
	return store.preview, nil
}

func (store *fakeAuthorizationStore) RequestSeal(_ context.Context, params taskstore.RequestSealParams) (taskstore.SealAdmission, error) {
	if store.fencer == nil || !store.fencer.isHeld() {
		return taskstore.SealAdmission{}, errors.New("request committed outside fence")
	}
	store.requestCalls++
	store.params = params
	return taskstore.SealAdmission{Request: taskstore.SealRequest{ID: params.SealRequestID}}, nil
}

func TestAuthorizerPreviewsAndCommitsExactSnapshotInsideFence(t *testing.T) {
	fencer := &fakeFencer{}
	store := &fakeAuthorizationStore{preview: authorizedWork().Preview, fencer: fencer}
	authorizer, err := NewAuthorizer(store, fencer, &fakeCollector{}, AuthorizerConfig{
		RepositoryPath: "/srv/fern/repository", PolicyVersion: "result-v1", OperationTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := authorizer.Preview(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ResultCommit != baseSHA || preview.TreeOID != treeSHA || preview.TaskRevision != 11 || preview.AttemptRevision != 13 || !preview.WorktreeClean {
		t.Fatalf("preview = %+v", preview)
	}
	params := taskstore.RequestSealParams{TaskID: taskID}
	if _, err := authorizer.Request(context.Background(), preview, params); err != nil {
		t.Fatal(err)
	}
	if store.requestCalls != 1 || store.params.ExpectedResultCommit != preview.ResultCommit ||
		store.params.ExpectedManifestSHA256 != preview.ManifestSHA256 || fencer.isHeld() || fencer.releases != 2 {
		t.Fatalf("calls=%d params=%+v held=%v releases=%d", store.requestCalls, store.params, fencer.isHeld(), fencer.releases)
	}
}

func TestAuthorizerRejectsChangedExpectedSnapshotBeforeAdmission(t *testing.T) {
	fencer := &fakeFencer{}
	store := &fakeAuthorizationStore{preview: authorizedWork().Preview, fencer: fencer}
	authorizer, err := NewAuthorizer(store, fencer, &fakeCollector{}, AuthorizerConfig{
		RepositoryPath: "/srv/fern/repository", PolicyVersion: "result-v1", OperationTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := authorizer.Preview(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	preview.TreeOID = baseSHA
	if _, err := authorizer.Request(context.Background(), preview, taskstore.RequestSealParams{TaskID: taskID}); !errors.Is(err, ErrSelectionChanged) {
		t.Fatalf("error = %v", err)
	}
	if store.requestCalls != 0 || fencer.isHeld() {
		t.Fatalf("requests=%d held=%v", store.requestCalls, fencer.isHeld())
	}
}
