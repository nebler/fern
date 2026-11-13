// Package envfile reads the node's settings files in /etc/fern, such as
// claude.env and backup.env: lines of NAME=value, which systemd reads too,
// as the EnvironmentFile of T3's unit.
package envfile

import (
	"os"
	"strings"
)

// Read returns the variables the file at path sets. A line without a = or
// that starts with # sets none, quotes around a value are dropped, and of a
// name set twice the last counts: what systemd makes of the same file, so
// the daemon and T3 never read two values from one. A file that isn't
// there is fs.ErrNotExist.
func Read(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vars := make(map[string]string)
	for line := range strings.Lines(string(data)) {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(name, "#") {
			vars[name] = strings.Trim(value, `"'`)
		}
	}
	return vars, nil
}
