package workspacegithub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeExecutor struct {
	output []byte
	err    error
	args   []string
}

func (f *fakeExecutor) Run(_ context.Context, arguments ...string) ([]byte, error) {
	f.args = append([]string(nil), arguments...)
	return append([]byte(nil), f.output...), f.err
}

func TestStatusUsesLiveTokenFreeProbe(t *testing.T) {
	executor := &fakeExecutor{output: []byte(`{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"fern-user","tokenSource":"/home/user/.config/gh/hosts.yml","scopes":"repo, read:org","gitProtocol":"https"}]}}`)}
	client, err := New(executor, "github.com")
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Login != "fern-user" || status.Hostname != "github.com" || status.GitProtocol != "https" || !reflect.DeepEqual(status.Scopes, []string{"repo", "read:org"}) {
		t.Fatalf("status = %+v", status)
	}
	want := []string{"auth", "status", "--active", "--hostname", "github.com", "--json", "hosts"}
	if !reflect.DeepEqual(executor.args, want) {
		t.Fatalf("args = %q, want %q", executor.args, want)
	}
}

func TestStatusFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   error
	}{
		{name: "empty", output: `{"hosts":{}}`, want: ErrUnauthenticated},
		{name: "revoked", output: `{"hosts":{"github.com":[{"state":"error","active":true,"host":"github.com","login":"fern-user"}]}}`, want: ErrUnavailable},
		{name: "timeout", output: `{"hosts":{"github.com":[{"state":"timeout","active":true,"host":"github.com","login":"fern-user"}]}}`, want: ErrUnavailable},
		{name: "inactive", output: `{"hosts":{"github.com":[{"state":"success","active":false,"host":"github.com","login":"fern-user"}]}}`, want: ErrUnauthenticated},
		{name: "token", output: `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"fern-user","token":"secret","gitProtocol":"https"}]}}`, want: ErrUnavailable},
		{name: "wrong host", output: `{"hosts":{"github.com":[{"state":"success","active":true,"host":"evil.example","login":"fern-user","gitProtocol":"https"}]}}`, want: ErrUnauthenticated},
		{name: "trailing", output: `{"hosts":{}} {}`, want: ErrUnavailable},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			client, _ := New(&fakeExecutor{output: []byte(test.output)}, "github.com")
			_, err := client.Status(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRepositoryRequiresExactIdentity(t *testing.T) {
	executor := &fakeExecutor{output: []byte(`{"id":123,"fullName":"owner/repository"}`)}
	client, _ := New(executor, "github.com")
	repository, err := client.Repository(context.Background(), "owner/repository")
	if err != nil || repository.ID != 123 {
		t.Fatalf("repository=%+v err=%v", repository, err)
	}
	if strings.Join(executor.args, " ") != `api --hostname github.com --method GET repos/owner/repository --jq {id: .id, fullName: .full_name}` {
		t.Fatalf("args = %q", executor.args)
	}
	executor.output = []byte(`{"id":123,"fullName":"other/repository"}`)
	if _, err := client.Repository(context.Background(), "owner/repository"); !errors.Is(err, ErrRepository) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestOutputAndInputBounds(t *testing.T) {
	client, _ := New(&fakeExecutor{output: []byte(strings.Repeat("x", maxOutputBytes+1))}, "github.com")
	if _, err := client.Status(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("large output error = %v", err)
	}
	if _, err := client.Repository(context.Background(), "../escape"); !errors.Is(err, ErrRepository) {
		t.Fatalf("invalid repository error = %v", err)
	}
	if _, err := New(&fakeExecutor{}, "enterprise.example"); err == nil {
		t.Fatal("New accepted unsupported hostname")
	}
}
