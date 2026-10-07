package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/store/storetest"
)

func TestDeviceRevokedBeforeAdmissionIsUnauthenticated(t *testing.T) {
	store := control.New(storetest.DB(t))
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
	response := httptest.NewRecorder()
	_, _, admitted := newPairingState(store).admit(response, httptest.NewRequest(http.MethodGet, "/fern/", nil), device, "device-token")
	if admitted || response.Code != http.StatusUnauthorized {
		t.Fatalf("admitted=%v status=%d, want 401 without admission", admitted, response.Code)
	}
}
