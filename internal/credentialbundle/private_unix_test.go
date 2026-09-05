//go:build darwin || linux

package credentialbundle

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPrivateReadersRejectUnsafeLeavesWithoutBlocking(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"symlink", "public", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret-must-not-leak")
			var err error
			switch kind {
			case "symlink":
				if err := os.WriteFile(path+"-target", []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(path+"-target", path)
			case "public":
				err = os.WriteFile(path, []byte("secret"), 0o600)
				if err == nil {
					err = os.Chmod(path, 0o644)
				}
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 2)
			go func() { _, err := ReadFile(path, nil); done <- err }()
			go func() { _, err := LoadIdentities([]string{path}); done <- err }()
			for range 2 {
				select {
				case err := <-done:
					if !errors.Is(err, ErrUnsafeFile) || err.Error() != ErrUnsafeFile.Error() {
						t.Fatalf("unsafe leaf error = %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("private file open blocked")
				}
			}
		})
	}
}
