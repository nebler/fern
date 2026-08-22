package workspacegithub

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	baseSHA = "1111111111111111111111111111111111111111"
	headSHA = "2222222222222222222222222222222222222222"
)

func validPullRequest() CreatePullRequest {
	return CreatePullRequest{RepositoryFullName: "owner/repository", RepositoryID: 123, BaseRef: "main", BaseSHA: baseSHA,
		HeadRef: "fern/demo/op_123", HeadSHA: headSHA, Title: "Draft title", Body: "Created by Fern."}
}

func pullJSON() string {
	return `{"number":42,"url":"https://github.com/owner/repository/pull/42","state":"open","draft":true,"baseRef":"main","baseSha":"` + baseSHA + `","headRef":"fern/demo/op_123","headSha":"` + headSHA + `","baseRepositoryId":123,"headRepositoryId":123}`
}

func TestCreateDraftPullRequestUsesFixedRESTArguments(t *testing.T) {
	executor := &fakeExecutor{output: []byte(pullJSON())}
	client, _ := New(executor, "github.com")
	pull, err := client.CreateDraftPullRequest(context.Background(), validPullRequest())
	if err != nil || pull.Number != 42 {
		t.Fatalf("pull=%+v err=%v", pull, err)
	}
	joined := strings.Join(executor.args, "\x00")
	for _, required := range []string{"api", "--method\x00POST", "repos/owner/repository/pulls", "title=Draft title", "head=fern/demo/op_123", "base=main", "draft=true"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("arguments %q do not contain %q", executor.args, required)
		}
	}
}

func TestFindDraftPullRequestReconcilesExactTuple(t *testing.T) {
	executor := &fakeExecutor{output: []byte("[" + pullJSON() + "]")}
	client, _ := New(executor, "github.com")
	pull, found, err := client.FindDraftPullRequest(context.Background(), validPullRequest())
	if err != nil || !found || pull.Number != 42 {
		t.Fatalf("pull=%+v found=%v err=%v", pull, found, err)
	}
	if !strings.Contains(strings.Join(executor.args, " "), "head=owner%3Afern%2Fdemo%2Fop_123") {
		t.Fatalf("query arguments = %q", executor.args)
	}
	executor.output = []byte(`[]`)
	if _, found, err := client.FindDraftPullRequest(context.Background(), validPullRequest()); err != nil || found {
		t.Fatalf("absent found=%v err=%v", found, err)
	}
}

func TestPullRequestConflictsFailClosed(t *testing.T) {
	request := validPullRequest()
	for _, mutate := range []func(*CreatePullRequest){
		func(value *CreatePullRequest) { value.RepositoryFullName = "../escape" },
		func(value *CreatePullRequest) { value.RepositoryID = 0 },
		func(value *CreatePullRequest) { value.BaseRef = "bad..ref" },
		func(value *CreatePullRequest) { value.HeadRef = "bad ref" },
		func(value *CreatePullRequest) { value.BaseSHA = "ABC" },
		func(value *CreatePullRequest) { value.Title = "" },
		func(value *CreatePullRequest) { value.Body = strings.Repeat("x", (64<<10)+1) },
	} {
		candidate := request
		mutate(&candidate)
		client, _ := New(&fakeExecutor{}, "github.com")
		if _, err := client.CreateDraftPullRequest(context.Background(), candidate); !errors.Is(err, ErrPullRequestConflict) {
			t.Fatalf("request %+v error = %v", candidate, err)
		}
	}

	mismatch := strings.Replace(pullJSON(), `"headSha":"`+headSHA+`"`, `"headSha":"`+baseSHA+`"`, 1)
	client, _ := New(&fakeExecutor{output: []byte(mismatch)}, "github.com")
	if _, err := client.CreateDraftPullRequest(context.Background(), request); !errors.Is(err, ErrPullRequestConflict) {
		t.Fatalf("mismatch error = %v", err)
	}
	client, _ = New(&fakeExecutor{output: []byte("[" + pullJSON() + "," + pullJSON() + "]")}, "github.com")
	if _, _, err := client.FindDraftPullRequest(context.Background(), request); !errors.Is(err, ErrPullRequestConflict) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestCreateErrorsAreUncertainAndRedacted(t *testing.T) {
	client, _ := New(&fakeExecutor{err: errors.New("gho_secret")}, "github.com")
	if _, err := client.CreateDraftPullRequest(context.Background(), validPullRequest()); !errors.Is(err, ErrPullRequestUnknown) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("create error = %v", err)
	}
}
