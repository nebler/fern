package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func TestRuntimePreservesExactIdentity(t *testing.T) {
	for _, started := range []string{"2026-09-05T01:02:03.123456789Z", "2026-09-05T01:02:03+02:00"} {
		r, err := NewRuntime("container", started)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("container\x00" + started))
		parsed, _ := time.Parse(time.RFC3339Nano, started)
		if r.ContainerID() != "container" || r.StartedAt() != started || r.Token() != hex.EncodeToString(sum[:]) || r.Epoch() != parsed.UnixNano() {
			t.Fatalf("identity changed: %+v", r)
		}
		if got, err := ParseRuntime(r.ContainerID(), r.StartedAt(), r.Token()); err != nil || got != r {
			t.Fatalf("roundtrip = %+v, %v", got, err)
		}
		if _, err := ParseRuntime(r.ContainerID(), r.StartedAt(), "wrong"); err == nil {
			t.Fatal("accepted wrong token")
		}
	}
}

func TestRuntimeRejectsNoncanonicalIdentity(t *testing.T) {
	for _, started := range []string{"", "not-time", "0001-01-01T00:00:00Z", "2026-09-05T01:02:03.100Z", "2026-09-05T01:02:03+00:00"} {
		if _, err := NewRuntime("container", started); err == nil {
			t.Fatalf("accepted %q", started)
		}
	}
	if _, err := NewRuntime("", "2026-09-05T01:02:03Z"); err == nil {
		t.Fatal("accepted empty container")
	}
	if _, err := ParseRuntime("", "", ""); err == nil {
		t.Fatal("accepted zero runtime")
	}
}
