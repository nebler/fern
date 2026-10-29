package setup_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/nebler/fern/internal/setup"
)

// FuzzKeyStaysInRepo checks that whatever services.yaml lists under
// golden.inputs, the golden key reads nothing outside the repository: the
// file is the agent's, and the key is computed by the daemon, as root.
// Changing a file beside the repository never changes the key.
func FuzzKeyStaysInRepo(f *testing.F) {
	for _, seed := range []string{
		"services:\n  db:\n    run: postgres\ngolden:\n  inputs: [seed.sql]\n",
		"golden:\n  inputs: [../outside]\n",
		"golden:\n  inputs: [/etc/passwd]\n",
		"golden:\n  inputs: [sub/../../outside]\n",
		"golden:\n  inputs: [link]\n",
		"golden: {inputs: [\"\"]}\n",
		"not: [valid",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, servicesYAML string) {
		dir := t.TempDir()
		repo := filepath.Join(dir, "repo")
		outside := filepath.Join(dir, "outside")
		if err := os.Mkdir(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "services.yaml"), []byte(servicesYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
			t.Fatal(err)
		}
		calls := 0
		key := func() string {
			calls++
			if err := os.WriteFile(outside, []byte(strconv.Itoa(calls)), 0o644); err != nil {
				t.Fatal(err)
			}
			k, err := setup.Key(repo, "image")
			if err != nil {
				return "error"
			}
			return k
		}
		if first, second := key(), key(); first != second {
			t.Errorf("Key changed (%s, then %s) when only a file outside the repository did", first, second)
		}
	})
}
