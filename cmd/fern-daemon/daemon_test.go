package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileLogin(t *testing.T) {
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
			got, err := fileLogin(file)(t.Context())
			if err != nil || got != tc.want {
				t.Errorf("fileLogin() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
