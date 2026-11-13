package envfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/envfile"
)

func TestRead(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		want       map[string]string
	}{
		{name: "empty", file: "", want: map[string]string{}},
		{name: "as install.sh writes it", file: "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-x\n", want: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-x"}},
		{
			name: "as a person writes it",
			file: "# where backups go\n\n  FERN_BACKUP_BUCKET=\"fern\"\nFERN_BACKUP_REGION='eu'\r\nFERN_BACKUP_ENDPOINT=https://s3.example/?a=b\nnot a variable\n",
			want: map[string]string{"FERN_BACKUP_BUCKET": "fern", "FERN_BACKUP_REGION": "eu", "FERN_BACKUP_ENDPOINT": "https://s3.example/?a=b"},
		},
		{name: "a comment that sets nothing", file: "#TOKEN=old\n", want: map[string]string{}},
		{name: "set twice", file: "TOKEN=old\nTOKEN=new\n", want: map[string]string{"TOKEN": "new"}},
		{name: "no newline at the end", file: "TOKEN=x", want: map[string]string{"TOKEN": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.env")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := envfile.Read(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Read() (-want +got):\n%s", diff)
			}
		})
	}
	if _, err := envfile.Read(filepath.Join(t.TempDir(), "none.env")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read() of a file that isn't there: error %v, want %v", err, fs.ErrNotExist)
	}
}
