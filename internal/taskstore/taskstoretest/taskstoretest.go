// Package taskstoretest opens throwaway Fern state databases for tests of the
// packages whose tables live in the taskstore schema.
package taskstoretest

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/nebler/fern/internal/taskstore"
)

// Open initializes a fresh store in a private temporary directory and closes
// it when the test ends. It also returns the database file path.
func Open(t testing.TB) (*taskstore.Store, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.db")
	store, err := taskstore.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

// DB opens a fresh store like Open and returns only its shared handle, for
// the packages (control, pluginauth) that issue their own SQL.
func DB(t testing.TB) *sql.DB {
	t.Helper()
	store, _ := Open(t)
	return store.DB()
}

// AssertNoSecrets fails if any secret appears in the database file or its
// write-ahead log.
func AssertNoSecrets(t testing.TB, path string, secrets ...string) {
	t.Helper()
	for _, file := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(file)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(data, []byte(secret)) {
				t.Fatalf("%s contains secret %q", filepath.Base(file), secret)
			}
		}
	}
}
