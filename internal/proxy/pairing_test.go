package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
)

func TestServePairedRejectsDeviceRevokedBeforeDispatch(t *testing.T) {
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	device, err := store.AddDevice("device-token", "phone", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Authentication already returned device; revocation lands before the
	// request is registered against it.
	if err := store.RevokeDevice(device.ID); err != nil {
		t.Fatal(err)
	}
	state := &pairingState{store: store}
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	response := httptest.NewRecorder()
	state.servePaired(response, httptest.NewRequest(http.MethodGet, "/fern/", nil), next, device, "device-token")
	if called || response.Code != http.StatusUnauthorized {
		t.Fatalf("called=%v status=%d, want 401 without dispatch", called, response.Code)
	}
}
