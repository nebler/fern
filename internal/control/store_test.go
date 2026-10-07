package control

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/taskstore/taskstoretest"
)

func TestStorePersistsDevices(t *testing.T) {
	database, path := taskstoretest.Open(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	device, err := New(database.DB()).AddDevice("device-secret", "Noah's phone", now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	taskstoretest.AssertNoSecrets(t, path, "device-secret")

	reopened := New(database.DB())
	if valid, err := authenticateDevice(reopened, "device-secret", now.Add(time.Minute)); err != nil || !valid {
		t.Fatalf("persisted authentication valid=%t err=%v", valid, err)
	}
	devices, err := reopened.Devices(now)
	if err != nil || len(devices) != 1 || devices[0] != device {
		t.Fatalf("devices=%+v err=%v", devices, err)
	}
	if err := reopened.RevokeDevice(device.ID); err != nil {
		t.Fatal(err)
	}
	if valid, err := authenticateDevice(reopened, "device-secret", now); err != nil || valid {
		t.Fatalf("revoked authentication valid=%t err=%v", valid, err)
	}
}

func TestStorePrunesExpiredDevice(t *testing.T) {
	store := New(taskstoretest.DB(t))
	now := time.Now()
	if _, err := store.AddDevice("expired", "old", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if valid, err := authenticateDevice(store, "expired", now.Add(2*time.Minute)); err != nil || valid {
		t.Fatalf("expired authentication valid=%t err=%v", valid, err)
	}
}

func TestStoreAuthenticationReturnsDurableDeviceIdentity(t *testing.T) {
	store := New(taskstoretest.DB(t))
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	want, err := store.AddDevice("device-secret", "Phone", now, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, valid, err := store.AuthenticateDeviceIdentity("device-secret", now.Add(time.Minute))
	if err != nil || !valid || got != want {
		t.Fatalf("identity=%+v valid=%t err=%v, want %+v", got, valid, err, want)
	}
	if got, valid, err := store.AuthenticateDeviceIdentity("wrong-secret", now); err != nil || valid || got != (Device{}) {
		t.Fatalf("invalid identity=%+v valid=%t err=%v", got, valid, err)
	}
	refreshed, valid, err := store.AuthenticateDeviceIdentity("device-secret", now.Add(61*time.Minute))
	if err != nil || !valid || !refreshed.LastSeen.Equal(now.Add(61*time.Minute)) {
		t.Fatalf("hourly refresh=%+v valid=%t err=%v", refreshed, valid, err)
	}
}

func TestDeviceLimitAdmitsAfterExpiredDevicesArePruned(t *testing.T) {
	store := New(taskstoretest.DB(t))
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	for i := range maxDevices {
		expires := now.Add(time.Hour)
		if i == 0 {
			expires = now.Add(time.Minute)
		}
		if _, err := store.AddDevice(fmt.Sprintf("token-%d", i), "Phone", now, expires); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AddDevice("one-too-many", "Phone", now, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "device limit") {
		t.Fatalf("device over limit error = %v", err)
	}
	if _, err := store.AddDevice("one-too-many", "Phone", now.Add(time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatalf("device after pruning = %v", err)
	}
}

func TestEnsureOperatorCredentialIDIsStableRandomAndPersisted(t *testing.T) {
	database, _ := taskstoretest.Open(t)
	first, err := New(database.DB()).EnsureOperatorCredentialID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "control-") || len(first) != len("control-")+26 {
		t.Fatalf("operator credential ID %q has unexpected format", first)
	}
	if persisted, err := New(database.DB()).EnsureOperatorCredentialID(); err != nil || persisted != first {
		t.Fatalf("persisted ID = %q, %v; want %q", persisted, err, first)
	}
	if different, err := New(taskstoretest.DB(t)).EnsureOperatorCredentialID(); err != nil || different == first {
		t.Fatalf("independent store reused ID %q (%v)", different, err)
	}
}
