package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/nebler/fern/internal/runsc"
)

func TestFileVar(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, data, want string
	}{
		{name: "missing", want: ""},
		{name: "token", data: "CLAUDE_CODE_OAUTH_TOKEN=sk-1\n", want: "sk-1"},
		{name: "among others", data: "# login\nOTHER=x\n  CLAUDE_CODE_OAUTH_TOKEN=sk-2  \n", want: "sk-2"},
		{name: "none in it", data: "OTHER=x\n", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(dir, tc.name)
			if tc.data != "" {
				if err := os.WriteFile(file, []byte(tc.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := fileVar(file, "CLAUDE_CODE_OAUTH_TOKEN")(t.Context())
			if err != nil || got != tc.want {
				t.Errorf("fileVar() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestFileSkills(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, data, want string }{
		{name: "missing", want: "safety"},
		{name: "packs", data: "# Claude's skills\nbeginner design\nengineering # and more\n", want: "beginner design engineering"},
		{name: "none", data: "# none\n", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(dir, tc.name)
			if tc.data != "" {
				if err := os.WriteFile(file, []byte(tc.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := fileSkills(file, "safety")(t.Context()); err != nil || got != tc.want {
				t.Errorf("fileSkills() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestLimitFlags(t *testing.T) {
	def := runsc.DefaultLimits()
	for _, tc := range []struct {
		name         string
		args         []string
		limits, most runsc.Limits
		wantErr      bool
	}{
		{name: "none", limits: def, most: def},
		{
			name: "a size", args: []string{"--box-cpus=1.5", "--box-memory=512", "--box-pids=300"},
			limits: runsc.Limits{CPUs: 1.5, Memory: 512 << 20, Pids: 300}, most: runsc.Limits{CPUs: 1.5, Memory: 512 << 20, Pids: 300},
		},
		{
			name: "a most", args: []string{"--box-max-memory=8192"},
			limits: def, most: runsc.Limits{CPUs: def.CPUs, Memory: 8 << 30, Pids: def.Pids},
		},
		{name: "too small", args: []string{"--box-memory=1"}, wantErr: true},
		{name: "a most under the size", args: []string{"--box-max-pids=100"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
			read := limitFlags(flags)
			if err := flags.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			limits, most, err := read()
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %t", err, tc.wantErr)
			}
			if err == nil && (limits != tc.limits || most != tc.most) {
				t.Errorf("limits %+v, most %+v; want %+v, %+v", limits, most, tc.limits, tc.most)
			}
		})
	}
}

// A box can stand in for T3's MCP server only at an address on its own
// loopback: T3 anywhere else gets no listener.
func TestT3MCP(t *testing.T) {
	for _, tc := range []struct{ t3, want string }{
		{"http://127.0.0.1:30773", "127.0.0.1:30773"},
		{"http://[::1]:30773", "[::1]:30773"},
		{"http://10.0.0.5:30773", ""},
		{"http://t3.example.com:30773", ""},
		{"http://127.0.0.1", ""},
		{"", ""},
	} {
		if got := t3MCP(tc.t3); got != tc.want {
			t.Errorf("t3MCP(%q) = %q, want %q", tc.t3, got, tc.want)
		}
	}
}
