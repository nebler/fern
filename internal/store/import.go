package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // the "sqlite" driver, to read the index of an earlier Fern
)

// legacyIndex is the SQLite index Fern kept until 2026-11-12, under the
// pool: a row per environment and per snapshot. A pool made before then
// has one, and so does a backup taken before then, which a restore unpacks
// to the same place.
const legacyIndex = "state/fern.sqlite"

// importIndex writes a record for every environment in the legacy index
// that has a directory and no record yet, then moves the index aside. Its
// snapshot rows aren't read: the snapshots on disk say what exists and when
// each was taken. It is safe to repeat: a stop half-way leaves the index
// where it was, and the next Open imports what is still missing.
func (s *Store) importIndex(ctx context.Context) error {
	old := filepath.Join(s.root, legacyIndex)
	if _, err := os.Lstat(old); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// The index held every box token, and so does the copy kept of it.
	if err := os.Chmod(filepath.Dir(old), 0o700); err != nil {
		return err
	}
	if err := s.importRows(ctx, old); err != nil {
		return fmt.Errorf("import %s: %w", old, err)
	}
	// Closing its last connection folded the index's log into it.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(old + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.Rename(old, old+".imported")
}

func (s *Store) importRows(ctx context.Context, old string) (err error) {
	db, err := sql.Open("sqlite", "file:"+old+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	// Its box tokens aren't read: a box's token is made at its start now, and
	// a box running since before answers to the one in its bundle.
	rows, err := db.QueryContext(ctx, `SELECT id, workspace, git, created_at FROM environments`)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id string
		var created int64
		var rec record
		if err := rows.Scan(&id, &rec.Workspace, &rec.Git, &created); err != nil {
			return err
		}
		if CheckEnvID(id) != nil {
			continue
		}
		// A row without its directory describes nothing a box could use.
		if _, err := os.Lstat(s.envDir(id)); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, err := os.Lstat(s.recordPath(id)); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		rec.Created = time.Unix(created, 0).UTC()
		if err := s.writeRecord(id, rec); err != nil {
			return err
		}
	}
	return rows.Err()
}
