package main

import (
	"bytes"
	"strings"
	"testing"
)

// git asks for credentials on stdin; only github.com URLs with a path get a
// token, and none is an empty answer, so git goes on without one.
func TestGitCredential(t *testing.T) {
	d := newFakeDaemon(t)
	tests := []struct {
		name, in, want string
	}{
		{"a repository with the App", "protocol=https\nhost=github.com\npath=owner/app.git\n\n", "username=x-access-token\npassword=ghs_x\n"},
		{"one without", "protocol=https\nhost=github.com\npath=owner/other.git\n\n", ""},
		{"another host", "protocol=https\nhost=gitlab.com\npath=owner/app.git\n\n", ""},
		{"no path", "protocol=https\nhost=github.com\n\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(t.Context(), d.socket, []string{"git-credential", "get"}, strings.NewReader(tt.in), &out, &errOut)
			if code != 0 || out.String() != tt.want {
				t.Errorf("fern git-credential get = %d, %q (%s); want 0, %q", code, out.String(), errOut.String(), tt.want)
			}
		})
	}
	var out bytes.Buffer
	if code := run(t.Context(), d.socket, []string{"git-credential", "store"}, strings.NewReader("password=x\n\n"), &out, &out); code != 0 || out.Len() != 0 {
		t.Errorf("fern git-credential store = %d, %q; want 0 and nothing", code, out.String())
	}
}

func TestGitHubToken(t *testing.T) {
	d := newFakeDaemon(t)
	var out, errOut bytes.Buffer
	if code := run(t.Context(), d.socket, []string{"github-token", "owner/app"}, nil, &out, &errOut); code != 0 || out.String() != "ghs_x\n" {
		t.Errorf("fern github-token owner/app = %d, %q (%s); want 0, ghs_x", code, out.String(), errOut.String())
	}
	if code := run(t.Context(), d.socket, []string{"github-token", "owner/other"}, nil, &out, &errOut); code != 1 {
		t.Errorf("fern github-token of a repository without the App = %d, want 1", code)
	}
	// What T3's gh gets outside a repository: the account's.
	out.Reset()
	if code := run(t.Context(), d.socket, []string{"github-token", "--account"}, nil, &out, &errOut); code != 0 || out.String() != "ghs_account\n" {
		t.Errorf("fern github-token --account = %d, %q (%s); want 0, ghs_account", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run(t.Context(), d.socket, []string{"github-token", "--account", "owner"}, nil, &out, &errOut); code != 0 || out.String() != "ghs_account\n" {
		t.Errorf("fern github-token --account owner = %d, %q; want 0, ghs_account", code, out.String())
	}
	if code := run(t.Context(), d.socket, []string{"github-token", "--account", "elsewhere"}, nil, &out, &errOut); code != 1 {
		t.Errorf("fern github-token --account of an account without the App = %d, want 1", code)
	}
}

func TestGitHubRepository(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/owner/app.git":    "owner/app",
		"https://github.com/owner/app":        "owner/app",
		"git@github.com:owner/app.git":        "owner/app",
		"ssh://git@github.com/owner/app.git":  "owner/app",
		"https://gitlab.com/owner/app.git":    "",
		"https://github.com/owner":            "",
		"https://github.com/owner/app/tree/x": "",
	} {
		got, ok := githubRepository(remote)
		if got != want || ok != (want != "") {
			t.Errorf("githubRepository(%q) = %q, %t; want %q", remote, got, ok, want)
		}
	}
}
