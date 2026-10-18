package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParse(t *testing.T) {
	const sock = defaultSocket
	tests := []struct {
		name    string
		args    []string
		envSock string // FERN_SOCKET
		want    command
		wantErr bool
	}{
		{
			name: "create with repo after env",
			args: []string{"create", "demo", "--repo", "dir"},
			want: command{name: "create", socket: sock, env: "demo", repo: "dir"},
		},
		{
			name: "create with repo before env",
			args: []string{"create", "--repo", "dir", "demo"},
			want: command{name: "create", socket: sock, env: "demo", repo: "dir"},
		},
		{
			name: "create from stdin",
			args: []string{"create", "demo", "--repo", "-"},
			want: command{name: "create", socket: sock, env: "demo", repo: "-"},
		},
		{
			name: "ls",
			args: []string{"ls"},
			want: command{name: "ls", socket: sock},
		},
		{
			name: "exec with --",
			args: []string{"exec", "demo", "--", "psql", "-c", "select 1"},
			want: command{name: "exec", socket: sock, env: "demo", argv: []string{"psql", "-c", "select 1"}},
		},
		{
			name: "exec -it",
			args: []string{"exec", "-it", "demo", "--", "bash"},
			want: command{name: "exec", socket: sock, env: "demo", stdin: true, tty: true, argv: []string{"bash"}},
		},
		{
			name: "exec -i -t",
			args: []string{"exec", "-i", "-t", "demo", "bash"},
			want: command{name: "exec", socket: sock, env: "demo", stdin: true, tty: true, argv: []string{"bash"}},
		},
		{
			name: "exec -i",
			args: []string{"exec", "-i", "demo", "cat"},
			want: command{name: "exec", socket: sock, env: "demo", stdin: true, argv: []string{"cat"}},
		},
		{
			name: "exec without --",
			args: []string{"exec", "demo", "ls", "-la"},
			want: command{name: "exec", socket: sock, env: "demo", argv: []string{"ls", "-la"}},
		},
		{
			name: "exec flags after env belong to the command",
			args: []string{"exec", "demo", "-i", "x"},
			want: command{name: "exec", socket: sock, env: "demo", argv: []string{"-i", "x"}},
		},
		{
			name: "snapshot with id",
			args: []string{"snapshot", "demo", "s1"},
			want: command{name: "snapshot", socket: sock, env: "demo", snap: "s1"},
		},
		{
			name: "snapshot without id",
			args: []string{"snapshot", "demo"},
			want: command{name: "snapshot", socket: sock, env: "demo"},
		},
		{
			name: "restore",
			args: []string{"restore", "demo", "s1"},
			want: command{name: "restore", socket: sock, env: "demo", snap: "s1"},
		},
		{
			name: "socket flag",
			args: []string{"--socket", "/tmp/f.sock", "rm", "demo"},
			want: command{name: "rm", socket: "/tmp/f.sock", env: "demo"},
		},
		{
			name:    "FERN_SOCKET",
			args:    []string{"wake", "demo"},
			envSock: "/tmp/env.sock",
			want:    command{name: "wake", socket: "/tmp/env.sock", env: "demo"},
		},
		{
			name:    "socket flag beats FERN_SOCKET",
			args:    []string{"-socket", "/tmp/f.sock", "sleep", "demo"},
			envSock: "/tmp/env.sock",
			want:    command{name: "sleep", socket: "/tmp/f.sock", env: "demo"},
		},
		{name: "no command", args: nil, wantErr: true},
		{name: "unknown command", args: []string{"frobnicate"}, wantErr: true},
		{name: "unknown flag", args: []string{"create", "--nope", "demo"}, wantErr: true},
		{name: "restore needs a snapshot", args: []string{"restore", "demo"}, wantErr: true},
		{name: "snapshot needs an env", args: []string{"snapshot"}, wantErr: true},
		{name: "rm needs an env", args: []string{"rm"}, wantErr: true},
		{name: "too many args", args: []string{"rm", "a", "b"}, wantErr: true},
		{name: "ls takes no args", args: []string{"ls", "demo"}, wantErr: true},
		{name: "exec needs a command", args: []string{"exec", "demo"}, wantErr: true},
		{name: "exec needs a command after --", args: []string{"exec", "demo", "--"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FERN_SOCKET", tt.envSock)
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
