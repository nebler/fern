package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorePersistsDevices(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	store, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.AddDevice("device-secret", "Noah's phone", now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(directory, tokenHash("demo")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateBytes, []byte("device-secret")) {
		t.Fatal("control state persisted a raw device token")
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatal(err)
	}
	if string(state["version"]) != "2" || state["workflows"] != nil || state["publications"] != nil {
		t.Fatalf("unexpected current schema: %s", stateBytes)
	}
	if !bytes.Contains(state["devices"], []byte(tokenHash("device-secret"))) {
		t.Fatal("control state did not persist the device token hash")
	}

	reopened, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if valid, err := authenticateDevice(reopened, "device-secret", now.Add(time.Minute)); err != nil || !valid {
		t.Fatalf("persisted authentication valid=%t err=%v", valid, err)
	}
	devices, err := reopened.Devices(now)
	if err != nil || len(devices) != 1 || devices[0].ID != device.ID {
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
	store, err := Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := store.AddDevice("expired", "old", now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if valid, err := authenticateDevice(store, "expired", now.Add(2*time.Minute)); err != nil || valid {
		t.Fatalf("expired authentication valid=%t err=%v", valid, err)
	}
}

func TestStoreAuthenticationReturnsDurableDeviceIdentity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	want, err := store.AddDevice("device-secret", "Phone", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, valid, err := store.AuthenticateDeviceIdentity("device-secret", now.Add(time.Minute))
	if err != nil || !valid || got != want {
		t.Fatalf("identity=%+v valid=%t err=%v, want %+v", got, valid, err, want)
	}
	if valid, err := authenticateDevice(store, "device-secret", now.Add(time.Minute)); err != nil || !valid {
		t.Fatalf("authentication valid=%t err=%v", valid, err)
	}
	if got, valid, err := store.AuthenticateDeviceIdentity("wrong-secret", now); err != nil || valid || got != (Device{}) {
		t.Fatalf("invalid identity=%+v valid=%t err=%v", got, valid, err)
	}
}

func TestEnsureOperatorCredentialIDIsStableRandomAndPersisted(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	store, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.EnsureOperatorCredentialID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "control-") || len(first) != len("control-")+base64.RawURLEncoding.EncodedLen(16) {
		t.Fatalf("operator credential ID %q has unexpected format", first)
	}
	if again, err := store.EnsureOperatorCredentialID(); err != nil || again != first {
		t.Fatalf("second call = %q, %v; want %q", again, err, first)
	}
	reopened, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if persisted, err := reopened.EnsureOperatorCredentialID(); err != nil || persisted != first {
		t.Fatalf("persisted ID = %q, %v; want %q", persisted, err, first)
	}
	other, err := Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if different, err := other.EnsureOperatorCredentialID(); err != nil || different == first {
		t.Fatalf("independent store reused ID %q (%v)", different, err)
	}
}

func TestLoadRejectsInvalidOperatorCredentialID(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, tokenHash("demo")+".json")
	data := `{"version":2,"workspace":"demo","operatorCredentialId":"control-not-base64","devices":{}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory, "demo"); err == nil || !strings.Contains(err.Error(), "invalid operator credential identifier") {
		t.Fatalf("Open error = %v, want invalid operator credential identifier", err)
	}
}

func TestStoreRejectsSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, "demo"); err == nil {
		t.Fatal("Open accepted symlink control directory")
	}
}

func TestAuxiliaryStatePathStaysBesideControlState(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	store, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.AuxiliaryStatePath("pairing")
	if err != nil || path != store.path+".pairing" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	for _, name := range []string{"", "../escape", "Pairing", "pairing-state", strings.Repeat("a", 33)} {
		if _, err := store.AuxiliaryStatePath(name); err == nil {
			t.Fatalf("accepted auxiliary state name %q", name)
		}
	}
}

func TestStoreRejectsUnsupportedStateWithoutMutation(t *testing.T) {
	for _, data := range []string{
		`{"version":1,"workspace":"demo","devices":{},"workflows":{},"publications":{}}`,
		`{"version":1,"workspace":"demo","devices":{}}`,
		`{"version":3,"workspace":"demo","devices":{}}`,
		`{"workspace":"demo","devices":{}}`,
	} {
		t.Run(data, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "control")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, tokenHash("demo")+".json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(directory, "demo"); err == nil || !strings.Contains(err.Error(), "unsupported Fern control state version") {
				t.Fatalf("Open error = %v, want unsupported version", err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(unchanged, []byte(data)) {
				t.Fatalf("rejected load modified state: err=%v", err)
			}
		})
	}
}

func TestDeviceRevocationRollsBackKnownWriteFailure(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	store, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	device, err := store.AddDevice("device-secret", "Phone", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	revision := store.data.Revision
	moved := directory + "-moved"
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeDevice(device.ID); err == nil {
		t.Fatal("revocation unexpectedly persisted through unavailable directory")
	}
	if store.data.Revision != revision {
		t.Fatal("failed revocation changed revision")
	}
	if got, valid, err := store.AuthenticateDeviceIdentity("device-secret", now); err != nil || !valid || got != device {
		t.Fatalf("failed revocation changed memory: device=%+v valid=%t err=%v", got, valid, err)
	}
	reopened, err := Open(moved, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got, valid, err := reopened.AuthenticateDeviceIdentity("device-secret", now); err != nil || !valid || got != device {
		t.Fatalf("failed revocation changed disk: device=%+v valid=%t err=%v", got, valid, err)
	}
}
