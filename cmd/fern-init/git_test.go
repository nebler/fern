package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Claude's box gets fern-git as its git and as git's own git, whatever the
// box was started with, and nothing else of its variables changes.
func TestGuardGit(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "the image's variables",
			env:  []string{"HOME=/tmp", "PATH=/usr/local/bin:/usr/bin", "FERN_SERVICES=off"},
			want: []string{
				"HOME=/tmp", "FERN_SERVICES=off",
				"PATH=/usr/local/libexec/fern-git-core:/usr/local/bin:/usr/bin",
				"GIT_EXEC_PATH=/usr/local/libexec/fern-git-core",
				"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit",
			},
		},
		{
			name: "no PATH, and git's variables set by someone else",
			env:  []string{"GIT_EXEC_PATH=/elsewhere", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=x"},
			want: []string{
				"PATH=/usr/local/libexec/fern-git-core:/usr/local/bin:/usr/bin:/bin",
				"GIT_EXEC_PATH=/usr/local/libexec/fern-git-core",
				"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.bareRepository", "GIT_CONFIG_VALUE_0=explicit",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, guardGit(tt.env)); diff != "" {
				t.Errorf("guardGit() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
