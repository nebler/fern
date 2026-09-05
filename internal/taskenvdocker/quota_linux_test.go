//go:build linux && (amd64 || arm64)

package taskenvdocker

import (
	"path/filepath"
	"testing"
)

func TestQuotaUnprovisionedDirectoryFailsClosed(t *testing.T) {
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspectProjectQuota(path); err == nil {
		t.Fatal("hermetic test directory unexpectedly has an operator-provisioned project quota")
	}
}
