package store_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/store"
)

// FuzzCheckEnvID checks that an ID CheckEnvID accepts is safe as a folder
// name under envs/ and as a box's hostname: one path element, no dot (an
// agent box's name adds .agent), lowercase, and at most 63 characters.
func FuzzCheckEnvID(f *testing.F) {
	for _, seed := range []string{"demo", "t3-8f2c", "../etc", "a/b", "demo.agent", "-x", "x-", "", strings.Repeat("a", 64), "Ab", "a\x00b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		if store.CheckEnvID(id) != nil {
			return
		}
		if id == "" || len(id) > 63 || strings.ContainsAny(id, "/.\\\x00") || strings.ToLower(id) != id || filepath.Base(id) != id {
			t.Errorf("CheckEnvID(%q) accepted an unsafe ID", id)
		}
	})
}
