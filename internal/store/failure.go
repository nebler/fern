package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// BuildFailure is a builder environment's last golden build, if it failed:
// what it was building, when, and why. It is a file in the builder's
// directory (envs/<id>/build-failed.json), so that a restart of the daemon
// keeps it: the next build of the same key waits (internal/actor), `fern
// doctor` lists it, and the repository's threads are told. A build that
// works removes it, and so does deleting the builder.
type BuildFailure struct {
	Key   string    `json:"key"` // the golden key that was being built
	At    time.Time `json:"at"`
	Error string    `json:"error"`
}

// maxFailure bounds the error kept: a setup script's last output is in it.
const maxFailure = 8 << 10

func (s *Store) failurePath(id string) string {
	return filepath.Join(s.envDir(id), "build-failed.json")
}

// SetBuildFailure records that builder environment id's build failed, or,
// with nil, that its last build worked. An environment that is gone has
// nothing to record.
func (s *Store) SetBuildFailure(_ context.Context, id string, f *BuildFailure) error {
	if err := CheckEnvID(id); err != nil {
		return err
	}
	if f == nil {
		return setFailure(s.failurePath(id), id, nil)
	}
	rec := *f
	rec.Error = rec.Error[:min(len(rec.Error), maxFailure)]
	return setFailure(s.failurePath(id), id, rec)
}

// BuildFailure returns builder environment id's last build's failure, and
// whether it has one.
func (s *Store) BuildFailure(_ context.Context, id string) (BuildFailure, bool, error) {
	if err := CheckEnvID(id); err != nil {
		return BuildFailure{}, false, err
	}
	var f BuildFailure
	found, err := readFailure(s.failurePath(id), &f)
	if errors.Is(err, errCutShort) {
		// It says only that a build failed.
		return BuildFailure{Error: "the record of the failure can't be read"}, true, nil
	}
	if err != nil {
		return BuildFailure{}, false, err
	}
	return f, found, nil
}

// SnapshotFailure is an environment's last snapshot, if it failed: which
// one, when, and why. T3 asks for a snapshot at each turn's checkpoint and
// goes on without it when it fails, so that turn then has no point to
// revert to, and nothing said so until someone tried. It is a file in the
// environment's directory (envs/<id>/snapshot-failed.json), beside its
// record and so in no snapshot, as a builder's failed build is: a restart
// of the daemon keeps it, `fern doctor` lists it, and the thread's Claude
// is told. The next snapshot that works removes it, and so does deleting
// the environment.
type SnapshotFailure struct {
	Snapshot string    `json:"snapshot"` // the snapshot that wasn't taken: T3's name for the turn's checkpoint
	At       time.Time `json:"at"`
	Error    string    `json:"error"`
}

func (s *Store) snapshotFailurePath(id string) string {
	return filepath.Join(s.envDir(id), "snapshot-failed.json")
}

// SetSnapshotFailure records that a snapshot of environment id failed, or,
// with nil, that its last one worked. An environment that is gone has
// nothing to record.
func (s *Store) SetSnapshotFailure(_ context.Context, id string, f *SnapshotFailure) error {
	if err := CheckEnvID(id); err != nil {
		return err
	}
	if f == nil {
		return setFailure(s.snapshotFailurePath(id), id, nil)
	}
	rec := *f
	rec.Error = rec.Error[:min(len(rec.Error), maxFailure)]
	return setFailure(s.snapshotFailurePath(id), id, rec)
}

// SnapshotFailure returns environment id's last snapshot's failure, and
// whether it has one.
func (s *Store) SnapshotFailure(_ context.Context, id string) (SnapshotFailure, bool, error) {
	if err := CheckEnvID(id); err != nil {
		return SnapshotFailure{}, false, err
	}
	var f SnapshotFailure
	found, err := readFailure(s.snapshotFailurePath(id), &f)
	if errors.Is(err, errCutShort) {
		// It says only that a snapshot failed.
		return SnapshotFailure{Error: "the record of the failure can't be read"}, true, nil
	}
	if err != nil {
		return SnapshotFailure{}, false, err
	}
	return f, found, nil
}

// setFailure makes rec the content of path, a failure's file in environment
// id's directory, or removes the file if rec is nil. Removing one that
// isn't there is not an error, so both are safe to repeat.
func setFailure(path, id string, rec any) error {
	if rec == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// One rename, so a reader sees the last failure or this one. It is not
	// flushed: a failure lost to a power cut costs one more build, or is
	// one snapshot's that nobody hears of. What a crash leaves beside it
	// (path + ".new") the next failure replaces, or Reconcile removes.
	next := path + ".new"
	if err := os.WriteFile(next, append(data, '\n'), 0o600); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("environment %q: %w", id, ErrNotFound)
		}
		return err
	}
	return os.Rename(next, path)
}

// errCutShort means a failure's file isn't JSON: a write that was cut short.
var errCutShort = errors.New("the record of the failure was cut short")

// readFailure reads the failure in path into rec, and reports whether
// there is one. A file that isn't JSON is errCutShort.
func readFailure(path string, rec any) (found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !json.Valid(data) {
		return true, errCutShort
	}
	return true, json.Unmarshal(data, rec)
}
