package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		envVars map[string]string // set while parsing
		want    command
		wantErr bool
	}{
		{
			name: "create",
			args: []string{"create", "demo"},
			want: command{name: "create", args: []string{"demo"}},
		},
		{
			// T3's checkpoint hook adopts a worktree like this.
			name: "create with a path",
			args: []string{"create", "--path", "/var/lib/fern/worktrees/r/b", "wt"},
			want: command{name: "create", args: []string{"wt"}, path: "/var/lib/fern/worktrees/r/b"},
		},
		{
			name: "create with a seed after the env",
			args: []string{"create", "demo", "--seed"},
			want: command{name: "create", args: []string{"demo"}, seed: true},
		},
		{
			name: "prebuild",
			args: []string{"prebuild", "sample"},
			want: command{name: "prebuild", args: []string{"sample"}},
		},
		{
			name: "ls",
			args: []string{"ls"},
			want: command{name: "ls"},
		},
		{
			name: "which",
			args: []string{"which", "/var/lib/fern/worktrees/r/b/src"},
			want: command{name: "which", args: []string{"/var/lib/fern/worktrees/r/b/src"}},
		},
		{
			name: "snapshot --replace",
			args: []string{"snapshot", "--replace", "demo", "s1"},
			want: command{name: "snapshot", args: []string{"demo", "s1"}, replace: true},
		},
		{
			name: "rmsnap",
			args: []string{"rmsnap", "demo", "s1", "s2"},
			want: command{name: "rmsnap", args: []string{"demo", "s1", "s2"}},
		},
		{
			name: "restore",
			args: []string{"restore", "demo", "s1"},
			want: command{name: "restore", args: []string{"demo", "s1"}},
		},
		{
			name: "exec with --",
			args: []string{"exec", "demo", "--", "psql", "-c", "select 1"},
			want: command{name: "exec", args: []string{"demo"}, argv: []string{"psql", "-c", "select 1"}},
		},
		{
			name: "exec -it",
			args: []string{"exec", "-it", "demo", "--", "bash"},
			want: command{name: "exec", args: []string{"demo"}, stdin: true, tty: true, argv: []string{"bash"}},
		},
		{
			name: "exec -i",
			args: []string{"exec", "-i", "demo", "cat"},
			want: command{name: "exec", args: []string{"demo"}, stdin: true, argv: []string{"cat"}},
		},
		{
			// What fern-claude runs; an unset variable passes nothing.
			name:    "exec --here -e",
			args:    []string{"exec", "-i", "--here", "-e", "FERN_TEST_SET", "-e", "FERN_TEST_UNSET", "--", "claude", "-p"},
			envVars: map[string]string{"FERN_TEST_SET": "yes"},
			want: command{
				name: "exec", stdin: true, here: true,
				environ: []string{"FERN_TEST_SET=yes"}, argv: []string{"claude", "-p"},
			},
		},
		{
			name: "exec flags after the env belong to the command",
			args: []string{"exec", "demo", "-i", "x"},
			want: command{name: "exec", args: []string{"demo"}, argv: []string{"-i", "x"}},
		},
		{
			name: "sleep",
			args: []string{"sleep", "demo"},
			want: command{name: "sleep", args: []string{"demo"}},
		},
		{name: "no command", args: nil, wantErr: true},
		{name: "unknown command", args: []string{"frobnicate"}, wantErr: true},
		{name: "unknown flag", args: []string{"create", "--nope", "demo"}, wantErr: true},
		{name: "snapshot needs a name", args: []string{"snapshot", "demo"}, wantErr: true},
		{name: "restore needs a snapshot", args: []string{"restore", "demo"}, wantErr: true},
		{name: "rm needs an env", args: []string{"rm"}, wantErr: true},
		{name: "too many args", args: []string{"rm", "a", "b"}, wantErr: true},
		{name: "ls takes no args", args: []string{"ls", "demo"}, wantErr: true},
		{name: "rmsnap needs a snapshot", args: []string{"rmsnap", "demo"}, wantErr: true},
		{name: "which needs a path", args: []string{"which"}, wantErr: true},
		{name: "exec needs a command", args: []string{"exec", "demo"}, wantErr: true},
		{name: "exec needs a command after --", args: []string{"exec", "demo", "--"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}
			got, err := parse(tt.args)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("parse(%q) error = %v, want error: %t", tt.args, err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.want, got, cmp.AllowUnexported(command{})); diff != "" {
				t.Errorf("parse(%q) mismatch (-want +got):\n%s", tt.args, diff)
			}
		})
	}
}
