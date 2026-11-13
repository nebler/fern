package runsc

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nebler/fern/internal/store"
)

// lastLogMax is how much of a stopped box's log is kept: its end, where a
// crash says why.
const lastLogMax = 1 << 20

// LastLog returns where the log of box's last run is kept in pool: in its
// environment's directory, as last.log, or last.agent.log for its agent box.
//
// It is there and not beside the running box's (boxes/<box>/) because an
// environment is its directory: deleting the environment removes the log
// with it, nothing else has to know that it exists, and a directory in
// boxes/ goes on meaning a box that may be running, which a wake and the
// driver's start look for.
func LastLog(pool, box string) string {
	env := store.EnvOfBox(box)
	return filepath.Join(pool, "envs", env, "last"+strings.TrimPrefix(box, env)+".log")
}

// keepLog keeps the end of a stopped box's log, the file from, as the log of
// its last run, the file to, replacing the one before: at most lastLogMax of
// it, from the start of a line.
//
// A stop calls it once the box is gone and before it removes the box's
// directory, so a stop that a crash cut short keeps it again: the copy
// appears in one rename, and what a crash leaves beside it (to + ".new") the
// next stop of the box overwrites. Nothing is kept of a box that has no log
// or an empty one (a wake that failed before the box was made, or a stop
// repeated), which leaves the log before; nor of a box whose environment's
// directory is gone, which a stop must not make again.
func keepLog(from, to string) error {
	f, err := os.Open(from)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	start := max(fi.Size()-lastLogMax, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, start, lastLogMax))
	if err != nil {
		return err
	}
	if start > 0 {
		// The first line is cut somewhere: leave it out.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	if len(data) == 0 {
		return nil
	}
	next := to + ".new"
	if err := os.WriteFile(next, data, 0o600); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the environment is gone
		}
		return err
	}
	return os.Rename(next, to)
}
