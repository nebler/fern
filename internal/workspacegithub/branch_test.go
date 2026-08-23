package workspacegithub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	branchBaseSHA = "1111111111111111111111111111111111111111"
	branchHeadSHA = "2222222222222222222222222222222222222222"
)

type commandStep struct {
	output string
	err    error
}

type commandScript struct {
	steps []commandStep
	args  [][]string
}

func (script *commandScript) Run(_ context.Context, arguments ...string) ([]byte, error) {
	script.args = append(script.args, append([]string(nil), arguments...))
	if len(script.steps) == 0 {
		return nil, errors.New("unexpected command")
	}
	step := script.steps[0]
	script.steps = script.steps[1:]
	return []byte(step.output), step.err
}

func validBranchRequest() BranchRequest {
	return BranchRequest{RepositoryFullName: "owner/repository", RepositoryID: 123, BaseRef: "main", BaseSHA: branchBaseSHA,
		HeadRef: "fern/demo/op_123", HeadSHA: branchHeadSHA}
}

func branchValidationSteps() []commandStep {
	return []commandStep{
		{output: `{"id":123,"fullName":"owner/repository"}`},
		{output: `{"name":"main","sha":"` + branchBaseSHA + `"}`},
	}
}

func TestReconcileBranchProvesBaseAndExactAbsence(t *testing.T) {
	script := &commandScript{steps: append(branchValidationSteps(), commandStep{output: `[]`})}
	client, _ := New(script, "github.com")
	observed, err := client.ReconcileBranch(context.Background(), validBranchRequest())
	if err != nil || observed.Exists {
		t.Fatalf("observation=%+v err=%v", observed, err)
	}
	wantRead := []string{"api", "--hostname", "github.com", "--method", "GET",
		"repos/owner/repository/git/matching-refs/heads/fern/demo/op_123", "--jq", `[.[] | {ref: .ref, sha: .object.sha}]`}
	if len(script.args) != 3 || !reflect.DeepEqual(script.args[2], wantRead) {
		t.Fatalf("commands = %q", script.args)
	}
}

func TestPushBranchOnceUsesFixedCreateAndReconcilesLostResponse(t *testing.T) {
	steps := branchValidationSteps()
	steps = append(steps,
		commandStep{output: `[]`},
		commandStep{err: errors.New("lost response with secret")},
		commandStep{output: `[{"ref":"refs/heads/fern/demo/op_123","sha":"` + branchHeadSHA + `"}]`},
	)
	script := &commandScript{steps: steps}
	client, _ := New(script, "github.com")
	proof, err := client.PushBranchOnce(context.Background(), validBranchRequest())
	if err != nil || !proof.Attempted || !proof.Observation.Exists || proof.Observation.SHA != branchHeadSHA {
		t.Fatalf("proof=%+v err=%v", proof, err)
	}
	wantCreate := []string{"api", "--hostname", "github.com", "--method", "POST", "repos/owner/repository/git/refs",
		"--raw-field", "ref=refs/heads/fern/demo/op_123", "--raw-field", "sha=" + branchHeadSHA,
		"--jq", `{ref: .ref, sha: .object.sha}`}
	if len(script.args) != 5 || !reflect.DeepEqual(script.args[3], wantCreate) {
		t.Fatalf("commands = %q", script.args)
	}
}

func TestPushBranchOnceNeverMutatesExistingRef(t *testing.T) {
	for _, test := range []struct {
		sha     string
		wantErr error
	}{
		{sha: branchHeadSHA},
		{sha: "3333333333333333333333333333333333333333", wantErr: ErrBranchConflict},
	} {
		steps := append(branchValidationSteps(), commandStep{output: `[{"ref":"refs/heads/fern/demo/op_123","sha":"` + test.sha + `"}]`})
		script := &commandScript{steps: steps}
		client, _ := New(script, "github.com")
		proof, err := client.PushBranchOnce(context.Background(), validBranchRequest())
		if !errors.Is(err, test.wantErr) || proof.Attempted || len(script.args) != 3 {
			t.Fatalf("sha=%s proof=%+v err=%v calls=%d", test.sha, proof, err, len(script.args))
		}
	}
}

func TestBranchRequestRejectsChangedBaseAndInvalidTuple(t *testing.T) {
	steps := []commandStep{
		{output: `{"id":123,"fullName":"owner/repository"}`},
		{output: `{"name":"main","sha":"3333333333333333333333333333333333333333"}`},
	}
	script := &commandScript{steps: steps}
	client, _ := New(script, "github.com")
	if _, err := client.ReconcileBranch(context.Background(), validBranchRequest()); !errors.Is(err, ErrBranchConflict) {
		t.Fatalf("moved base error = %v", err)
	}
	request := validBranchRequest()
	request.HeadRef = "../unsafe"
	if _, err := client.PushBranchOnce(context.Background(), request); !errors.Is(err, ErrBranchConflict) {
		t.Fatalf("invalid tuple error = %v", err)
	}
	if len(script.args) != 2 || strings.Contains(strings.Join(script.args[0], " "), "unsafe") {
		t.Fatalf("unexpected mutation commands = %q", script.args)
	}
}
