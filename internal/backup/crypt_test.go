package backup_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/nebler/fern/internal/backup"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, backup.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func seal(t *testing.T, k, plain []byte) []byte {
	t.Helper()
	r, err := backup.Encrypt(k, bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func open(k, sealed []byte) ([]byte, error) {
	r, err := backup.Decrypt(k, bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// What Encrypt seals, Decrypt opens, at every size around a chunk's.
func TestEncryptDecrypt(t *testing.T) {
	k := key(t)
	const chunk = 64 << 10
	for _, size := range []int{0, 1, chunk - 1, chunk, chunk + 1, 3 * chunk, 3*chunk + 17} {
		plain := make([]byte, size)
		_, _ = rand.Read(plain)
		got, err := open(k, seal(t, k, plain))
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("size %d: got %d bytes, %v; want the %d sealed", size, len(got), err, size)
		}
	}
}

// A changed byte, a stream cut at a chunk's end, a dropped chunk and another
// key all fail to open.
func TestDecryptRefuses(t *testing.T) {
	k := key(t)
	const sealedChunk = 64<<10 + 16
	plain := make([]byte, 3*(64<<10))
	sealed := seal(t, k, plain)
	flipped := bytes.Clone(sealed)
	flipped[100] ^= 1
	for name, tc := range map[string]struct {
		key, data []byte
	}{
		"a changed byte":  {k, flipped},
		"cut at a chunk":  {k, sealed[:7+sealedChunk]},
		"a dropped chunk": {k, append(bytes.Clone(sealed[:7+sealedChunk]), sealed[7+2*sealedChunk:]...)},
		"another key":     {key(t), sealed},
	} {
		if _, err := open(tc.key, tc.data); !errors.Is(err, backup.ErrKey) {
			t.Errorf("%s: %v, want ErrKey", name, err)
		}
	}
}
