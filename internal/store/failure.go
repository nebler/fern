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
	path := s.failurePath(id)
	if f == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	rec := *f
	if len(rec.Error) > maxFailure {
		rec.Error = rec.Error[:maxFailure]
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// One rename, so a reader sees the last failure or this one. It is not
	// flushed: a failure lost to a power cut costs one more build.
	next := path + ".new"
	if err := os.WriteFile(next, append(data, '\n'), 0o600); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("environment %q: %w", id, ErrNotFound)
		}
		return err
	}
	return os.Rename(next, path)
}

// BuildFailure returns builder environment id's last build's failure, and
// whether it has one.
func (s *Store) BuildFailure(_ context.Context, id string) (BuildFailure, bool, error) {
	if err := CheckEnvID(id); err != nil {
		return BuildFailure{}, false, err
	}
	data, err := os.ReadFile(s.failurePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return BuildFailure{}, false, nil
	}
	if err != nil {
		return BuildFailure{}, false, err
	}
	if !json.Valid(data) {
		// A file cut short says only that a build failed.
		return BuildFailure{Error: "the record of the failure can't be read"}, true, nil
	}
	var f BuildFailure
	if err := json.Unmarshal(data, &f); err != nil {
		return BuildFailure{}, false, err
	}
	return f, true, nil
}
