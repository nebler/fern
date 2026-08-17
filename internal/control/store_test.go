package control

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsDevicesAndWorkflows(t *testing.T) {
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
	workflow, err := store.CreateWorkflow("Fix signup", "session-123", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateWorkflow(workflow.ID, WorkflowCompleted, "", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	publication, created, err := store.RequestPublication(workflow.ID, Publication{
		ID: "publication-1", Operation: "operation-1", Title: "Fix signup",
	}, now.Add(2*time.Hour))
	if err != nil || !created {
		t.Fatalf("request publication=%+v created=%t err=%v", publication, created, err)
	}
	if err := store.PreparePublication(publication.ID, "owner/repo", "main", "fern/demo/operation-1", "0123456789012345678901234567890123456789", now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishPublication(publication.ID, "https://github.com/owner/repo/pull/1", "", now.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(directory, tokenHash("demo")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateBytes, []byte("device-secret")) {
		t.Fatal("control state persisted a raw device token")
	}

	reopened, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if valid, err := reopened.AuthenticateDevice("device-secret", now.Add(time.Minute)); err != nil || !valid {
		t.Fatalf("persisted authentication valid=%t err=%v", valid, err)
	}
	devices, err := reopened.Devices(now)
	if err != nil || len(devices) != 1 || devices[0].ID != device.ID {
		t.Fatalf("devices=%+v err=%v", devices, err)
	}
	got, exists := reopened.Workflow(workflow.ID)
	if !exists || got.Status != WorkflowPublished {
		t.Fatalf("workflow=%+v exists=%t", got, exists)
	}
	persistedPublication, exists := reopened.Publication(publication.ID)
	if !exists || persistedPublication.State != "published" || persistedPublication.PullURL == "" {
		t.Fatalf("publication=%+v exists=%t", persistedPublication, exists)
	}
	if err := reopened.RevokeDevice(device.ID); err != nil {
		t.Fatal(err)
	}
	if valid, err := reopened.AuthenticateDevice("device-secret", now); err != nil || valid {
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
	if valid, err := store.AuthenticateDevice("expired", now.Add(2*time.Minute)); err != nil || valid {
		t.Fatalf("expired authentication valid=%t err=%v", valid, err)
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

func TestStoreRejectsUnknownStateFields(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, tokenHash("demo")+".json")
	if err := os.WriteFile(path, []byte(`{"version":1,"devices":{},"workflows":{},"publications":{},"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory, "demo"); err == nil {
		t.Fatal("Open accepted unknown state field")
	}
}
