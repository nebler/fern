// Package storetest opens throwaway Fern state databases for tests of the
// packages whose tables live in the store schema.
package storetest

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/nebler/fern/internal/store"
)

// Open initializes a fresh store in a private temporary directory and closes
// it when the test ends. It also returns the database file path.
func Open(t testing.TB) (*store.Store, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "fern.db")
	runStore, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runStore.Close() })
	return runStore, path
}

// DB opens a fresh store like Open and returns only its shared handle, for
// package auth, which issues its own SQL.
func DB(t testing.TB) *sql.DB {
	t.Helper()
	runStore, _ := Open(t)
	return runStore.DB()
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
