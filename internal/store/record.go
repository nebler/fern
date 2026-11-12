package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// RecordFile is the name of an environment's record in its directory. An
// environment exists when its directory has one: Create writes it last and
// Delete removes it first.
const RecordFile = "env.json"

// recordVersion is the newest record format this Fern writes and reads.
const recordVersion = 1

// record is what an environment's directory can't say by itself: where its
// workspace is, which git folder its box gets and when it was made. Its ID
// is the directory's name, so the two can't disagree. It holds no secret: a
// box's token is made when the box starts and kept with its bundle, off the
// pool (runsc.Driver.Token). A record written before 2026-11-12 also has a
// box_token, which nothing reads any more.
type record struct {
	Version   int       `json:"version"`
	Workspace string    `json:"workspace"`
	Git       string    `json:"git"`
	Created   time.Time `json:"created"`
}

// errBadRecord means an environment's record is there and can't be used:
// it doesn't parse, or a newer Fern wrote it. The environment is left alone
// and not listed; Reconcile reports its directory.
var errBadRecord = errors.New("unreadable record")

// readRecord returns environment id's record, or an error that is
// fs.ErrNotExist if it has none and errBadRecord if it can't be used.
func (s *Store) readRecord(id string) (record, error) {
	data, err := os.ReadFile(s.recordPath(id))
	if err != nil {
		return record{}, err
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return record{}, fmt.Errorf("%s: %w: %w", s.recordPath(id), errBadRecord, err)
	}
	if rec.Version < 1 || rec.Version > recordVersion {
		return record{}, fmt.Errorf("%s: %w: version %d, and this Fern reads up to %d", s.recordPath(id), errBadRecord, rec.Version, recordVersion)
	}
	if rec.Workspace == "" {
		return record{}, fmt.Errorf("%s: %w: no workspace", s.recordPath(id), errBadRecord)
	}
	return rec, nil
}

// writeRecord makes rec environment id's record, replacing one it has. The
// whole record appears in one rename, flushed first: a power cut leaves the
// old record or the new one, and a worktree's path can't be worked out
// again from its directory. Only root reads it.
func (s *Store) writeRecord(id string, rec record) error {
	rec.Version = recordVersion
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := s.recordPath(id)
	next := path + ".new"
	f, err := os.OpenFile(next, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(next, path); err != nil {
		return err
	}
	dir, err := os.Open(s.envDir(id))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
