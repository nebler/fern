package backup

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StatusFile is where the last backup's result is kept, in the pool: a
// backup that fails is otherwise only a line in the daemon's log, and `fern
// doctor` said "set up" for backups that had failed for days.
const StatusFile = "backup.json"

// Status is how the node's last backup went.
type Status struct {
	At    time.Time `json:"at"`              // when it ended
	Error string    `json:"error,omitempty"` // why it failed; "" if it worked
	Good  time.Time `json:"good"`            // when the last one that worked ended; zero if none has
}

// ReadStatus returns the last backup's result in pool, and whether there has
// been a backup.
func ReadStatus(pool string) (Status, bool, error) {
	data, err := os.ReadFile(filepath.Join(pool, StatusFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, false, err
	}
	return s, true, nil
}

// record keeps how a backup that just ended went: err, or nil if it worked.
func (b *Backer) record(err error) error {
	now := time.Now().UTC()
	s := Status{At: now, Good: now}
	if err != nil {
		// A file that can't be read loses only when the last good one was.
		last, _, _ := ReadStatus(b.pool)
		s.Error, s.Good = err.Error(), last.Good
	}
	data, merr := json.Marshal(s)
	if merr != nil {
		return merr
	}
	path := filepath.Join(b.pool, StatusFile)
	if err := os.WriteFile(path+".new", append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}
