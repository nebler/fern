package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

// An object bigger than a part goes up in parts and comes back whole; it
// is listed until it is deleted.
func TestS3(t *testing.T) {
	_, s3 := newFakeS3(t)
	data := make([]byte, 2*partSize+123)
	_, _ = rand.Read(data)
	n, err := s3.Put(t.Context(), "fern/n/x", bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("Put() = %d, %v; want %d", n, err, len(data))
	}
	r, err := s3.Get(t.Context(), "fern/n/x")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("Get() = %d bytes, %v; want what was put", len(got), err)
	}
	if keys, err := s3.List(t.Context(), "fern/n/"); err != nil || len(keys) != 1 || keys[0] != "fern/n/x" {
		t.Errorf("List() = %q, %v; want [fern/n/x]", keys, err)
	}
	if err := s3.Delete(t.Context(), "fern/n/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Get(t.Context(), "fern/n/x"); !errors.Is(err, ErrNoObject) {
		t.Errorf("Get() after Delete: %v, want ErrNoObject", err)
	}
	if _, err := s3.Put(t.Context(), "fern/n/empty", bytes.NewReader(nil)); err != nil {
		t.Errorf("Put() of nothing: %v", err)
	}
}
