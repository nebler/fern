package backup

import (
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// The state goes up compressed and encrypted, and comes back into another
// pool: T3's database as SQLite copied it, T3's files, the repositories,
// the Claude folders, and the index where the daemon keeps it.
func TestStateRoundTrip(t *testing.T) {
	_, s3 := newFakeS3(t)
	cfg := &Config{S3: s3, Key: make([]byte, KeySize)}
	pool := t.TempDir()
	write := func(rel, data string) {
		t.Helper()
		p := filepath.Join(pool, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	write("userdata/settings.json", `{"a":1}`)
	write("repos/app/.git/HEAD", "ref: refs/heads/main\n")
	write("envs/e1/claude/session.jsonl", "{}\n")
	write("envs/e1/snaps/s/not-state", "x") // snapshots aren't state
	if err := os.Symlink("settings.json", filepath.Join(pool, "userdata", "link")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(pool, "userdata", "t3.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "create table threads (name text); insert into threads values ('one')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	index := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(index, []byte("the index"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := &Backer{cfg: cfg, prefix: "fern/n/", pool: pool, log: slog.New(slog.DiscardHandler)}
	tmp := t.TempDir()
	dbs, err := copyDatabases(t.Context(), filepath.Join(pool, "userdata"), tmp)
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeState(pw, pool, index, dbs)) }()
	if _, err := b.put(t.Context(), "fern/n/state/1", pr); err != nil {
		t.Fatal(err)
	}

	restored := t.TempDir()
	if err := extractState(t.Context(), cfg, "fern/n/state/1", restored); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		data, err := os.ReadFile(filepath.Join(restored, rel))
		if err != nil {
			return "missing"
		}
		return string(data)
	}
	got := map[string]string{}
	files := []string{
		"state/fern.sqlite", "userdata/settings.json", "userdata/link", "repos/app/.git/HEAD",
		"envs/e1/claude/session.jsonl", "envs/e1/snaps/s/not-state",
	}
	for _, rel := range files {
		got[rel] = read(rel)
	}
	want := map[string]string{
		"state/fern.sqlite": "the index", "userdata/settings.json": `{"a":1}`, "userdata/link": `{"a":1}`,
		"repos/app/.git/HEAD": "ref: refs/heads/main\n", "envs/e1/claude/session.jsonl": "{}\n", "envs/e1/snaps/s/not-state": "missing",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("restored files (-want +got):\n%s", diff)
	}
	db, err = sql.Open("sqlite", filepath.Join(restored, "userdata", "t3.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var name string
	if err := db.QueryRowContext(t.Context(), "select name from threads").Scan(&name); err != nil || name != "one" {
		t.Errorf("T3's database restored: %q, %v; want its row", name, err)
	}
}
