package workspacegithub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type scriptedExecutor struct {
	outputs [][]byte
	err     error
	args    [][]string
}

func (executor *scriptedExecutor) Run(_ context.Context, arguments ...string) ([]byte, error) {
	executor.args = append(executor.args, append([]string(nil), arguments...))
	if executor.err != nil {
		return nil, executor.err
	}
	if len(executor.outputs) == 0 {
		return nil, errors.New("unexpected gh command")
	}
	output := executor.outputs[0]
	executor.outputs = executor.outputs[1:]
	return append([]byte(nil), output...), nil
}

func TestBranchUsesGhForExactConfiguredRepository(t *testing.T) {
	executor := &scriptedExecutor{outputs: [][]byte{
		[]byte(`{"id":123,"fullName":"owner/repository"}`),
		[]byte(`{"name":"release/v1","sha":"0123456789abcdef0123456789abcdef01234567"}`),
	}}
	client, err := New(executor, "github.com")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := client.Branch(context.Background(), 123, "owner/repository", "release/v1")
	if err != nil || branch.SHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("branch=%+v err=%v", branch, err)
	}
	want := [][]string{
		{"api", "--hostname", "github.com", "--method", "GET", "repos/owner/repository", "--jq", `{id: .id, fullName: .full_name}`},
		{"api", "--hostname", "github.com", "--method", "GET", "repos/owner/repository/branches/release%2Fv1", "--jq", `{name: .name, sha: .commit.sha}`},
	}
	if !reflect.DeepEqual(executor.args, want) {
		t.Fatalf("commands = %q", executor.args)
	}
}

func TestBranchFailsClosedWithoutLeakingExecutorErrors(t *testing.T) {
	client, _ := New(&scriptedExecutor{err: errors.New("gho_secret")}, "github.com")
	if _, err := client.Branch(context.Background(), 123, "owner/repository", "main"); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v", err)
	}
	for _, ref := range []string{"../main", "bad ref", "refs//heads"} {
		if _, err := client.Branch(context.Background(), 123, "owner/repository", ref); !errors.Is(err, ErrRepository) {
			t.Fatalf("ref=%q error=%v", ref, err)
		}
	}
}

func TestBranchRejectsMismatchedGhOutput(t *testing.T) {
	for _, outputs := range [][][]byte{
		{[]byte(`{"id":124,"fullName":"owner/repository"}`)},
		{[]byte(`{"id":123,"fullName":"owner/repository"}`), []byte(`{"name":"other","sha":"0123456789abcdef0123456789abcdef01234567"}`)},
		{[]byte(`{"id":123,"fullName":"owner/repository"}`), []byte(`{"name":"main","sha":"bad"}`)},
	} {
		client, _ := New(&scriptedExecutor{outputs: outputs}, "github.com")
		if _, err := client.Branch(context.Background(), 123, "owner/repository", "main"); !errors.Is(err, ErrRepository) {
			t.Fatalf("outputs=%q error=%v", outputs, err)
		}
	}
}
