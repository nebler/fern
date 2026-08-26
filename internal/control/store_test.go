package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	prepared := testPreparedPublication("fern/demo/operation-1")
	if err := store.PreparePublication(publication.ID, prepared, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pull := testPullRequestObservation(prepared)
	if _, err := store.FinishPublication(publication.ID, &pull, "", now.Add(4*time.Hour)); err != nil {
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
	if valid, err := authenticateDevice(reopened, "device-secret", now.Add(time.Minute)); err != nil || !valid {
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
		t.Fatalf("legacy authentication valid=%t err=%v", valid, err)
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
	data := `{"version":1,"workspace":"demo","operatorCredentialId":"control-not-base64","devices":{},"workflows":{},"publications":{}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory, "demo"); err == nil {
		t.Fatal("Open accepted an invalid operator credential ID")
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

func TestPublishedPublicationCannotRegressToFailure(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	workflow, err := store.CreateWorkflow("Demo", "ses_demo", now)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.RequestPublication(workflow.ID, Publication{ID: "pub-1", Operation: "operation", Title: "Demo"}, now)
	if err != nil {
		t.Fatal(err)
	}
	prepared := testPreparedPublication("fern/demo/operation")
	if err := store.PreparePublication(record.ID, prepared, now); err != nil {
		t.Fatal(err)
	}
	pull := testPullRequestObservation(prepared)
	if _, err := store.FinishPublication(record.ID, &pull, "", now); err != nil {
		t.Fatal(err)
	}
	result, err := store.FinishPublication(record.ID, nil, "stale worker failed", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "published" || result.PullURL == "" {
		t.Fatalf("published state regressed: %+v", result)
	}
}

func TestPreparePublicationRetryMustMatchDurableRecord(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	workflow, err := store.CreateWorkflow("Demo", "ses_exact", now)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.RequestPublication(workflow.ID, Publication{ID: "pub-exact", Operation: "operation", Title: "Demo"}, now)
	if err != nil {
		t.Fatal(err)
	}
	prepared := testPreparedPublication("fern/demo/operation")
	if err := store.PreparePublication(record.ID, prepared, now); err != nil {
		t.Fatal(err)
	}
	different := prepared
	different.Branch = "fern/demo/other"
	if err := store.PreparePublication(record.ID, different, now); err == nil {
		t.Fatal("PreparePublication accepted a different retry tuple")
	}
	got, _ := store.Publication(record.ID)
	if got.Branch != "fern/demo/operation" || got.ResultCommit != prepared.ResultCommit {
		t.Fatalf("durable prepared record changed: %+v", got)
	}
}

func TestFinishPublicationRequiresEveryProofField(t *testing.T) {
	mutations := map[string]func(*PullRequestObservation){
		"target repository ID":      func(value *PullRequestObservation) { value.TargetRepositoryID++ },
		"target full name":          func(value *PullRequestObservation) { value.TargetRepositoryFullName = "owner/other" },
		"number":                    func(value *PullRequestObservation) { value.Number = 0 },
		"URL":                       func(value *PullRequestObservation) { value.URL += "/files" },
		"state":                     func(value *PullRequestObservation) { value.State = "closed" },
		"draft":                     func(value *PullRequestObservation) { value.Draft = false },
		"base repository ID":        func(value *PullRequestObservation) { value.Base.RepositoryID++ },
		"base full name":            func(value *PullRequestObservation) { value.Base.RepositoryFullName = "owner/other" },
		"base owner":                func(value *PullRequestObservation) { value.Base.RepositoryOwner = "other" },
		"base name":                 func(value *PullRequestObservation) { value.Base.RepositoryName = "other" },
		"base ref":                  func(value *PullRequestObservation) { value.Base.Ref = "other" },
		"base SHA":                  func(value *PullRequestObservation) { value.Base.SHA = strings.Repeat("c", 40) },
		"head repository ID":        func(value *PullRequestObservation) { value.Head.RepositoryID++ },
		"head repository full name": func(value *PullRequestObservation) { value.Head.RepositoryFullName = "owner/other" },
		"head repository owner":     func(value *PullRequestObservation) { value.Head.RepositoryOwner = "other" },
		"head repository name":      func(value *PullRequestObservation) { value.Head.RepositoryName = "other" },
		"head ref":                  func(value *PullRequestObservation) { value.Head.Ref = "other" },
		"head SHA":                  func(value *PullRequestObservation) { value.Head.SHA = strings.Repeat("c", 40) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "control"), "demo")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			workflow, err := store.CreateWorkflow("Demo", "ses_"+strings.ReplaceAll(name, " ", "_"), now)
			if err != nil {
				t.Fatal(err)
			}
			record, _, err := store.RequestPublication(workflow.ID, Publication{ID: "pub", Operation: "operation", Title: "Demo"}, now)
			if err != nil {
				t.Fatal(err)
			}
			prepared := testPreparedPublication("fern/demo/operation")
			if err := store.PreparePublication(record.ID, prepared, now); err != nil {
				t.Fatal(err)
			}
			pull := testPullRequestObservation(prepared)
			mutate(&pull)
			if _, err := store.FinishPublication(record.ID, &pull, "", now); err == nil {
				t.Fatal("FinishPublication accepted mismatched proof")
			}
			got, _ := store.Publication(record.ID)
			if got.State == PublicationPublished || got.PullRequest != nil {
				t.Fatalf("mismatched proof was persisted: %+v", got)
			}
		})
	}
}

func TestLegacyPublicationsRemainReadableWithoutMigration(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, tokenHash("demo")+".json")
	data := `{"version":1,"workspace":"demo","devices":{},"workflows":{},"publications":{"terminal":{"id":"terminal","workflowId":"wf","state":"published","operation":"op","title":"old","pullUrl":"https://github.com/owner/repo/pull/1","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"},"flight":{"id":"flight","workflowId":"wf","state":"pushing","operation":"op","title":"old","repository":"owner/repo","base":"main","branch":"fern/demo/op","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	terminal, _ := store.Publication("terminal")
	flight, _ := store.Publication("flight")
	if terminal.SchemaVersion != 0 || terminal.PullURL == "" || flight.SchemaVersion != 0 || flight.State != PublicationPrepared {
		t.Fatalf("legacy records changed: terminal=%+v flight=%+v", terminal, flight)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(unchanged, []byte(data)) {
		t.Fatalf("legacy load rewrote state: err=%v", err)
	}
}

func TestQuarantineLegacyPublicationsIsExplicitAtomicAndIdempotent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	quarantinedAt := time.Date(2026, 8, 26, 12, 30, 0, 0, time.UTC)
	alreadyQuarantined := Publication{
		ID: "already", WorkflowID: "wf", State: PublicationQuarantined, Operation: "op", Title: "old",
		OriginalState: PublicationFailed, QuarantinedAt: created.Add(time.Hour), QuarantineReason: LegacyPublicationQuarantineReason,
		CreatedAt: created, UpdatedAt: created.Add(time.Hour),
	}
	published := Publication{
		ID: "published", WorkflowID: "wf", State: PublicationPublished, Operation: "op", Title: "published",
		PullURL: "https://github.com/owner/repo/pull/1", CreatedAt: created, UpdatedAt: created,
	}
	state := diskState{
		Version: schemaVersion, Workspace: "demo", Devices: map[string]Device{}, Workflows: map[string]Workflow{},
		Publications: map[string]Publication{
			"legacy-requested":  {ID: "legacy-requested", WorkflowID: "wf", State: PublicationRequested, Operation: "one", Title: "one", CreatedAt: created, UpdatedAt: created},
			"current-requested": {SchemaVersion: PublicationSchemaVersion, ID: "current-requested", WorkflowID: "wf", State: PublicationRequested, Operation: "two", Title: "two", CreatedAt: created, UpdatedAt: created},
			"legacy-pushing":    {ID: "legacy-pushing", WorkflowID: "wf", State: PublicationPrepared, Operation: "three", Title: "three", Repository: "owner/repo", Base: "main", Branch: "fern/demo/three", Commit: strings.Repeat("a", 40), CreatedAt: created, UpdatedAt: created},
			"legacy-failed":     {ID: "legacy-failed", WorkflowID: "wf", State: PublicationFailed, Operation: "four", Title: "four", Error: "old failure", CreatedAt: created, UpdatedAt: created},
			"published":         published,
			"already":           alreadyQuarantined,
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, tokenHash("demo")+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !store.HasUnquarantinedLegacyPublications() {
		t.Fatal("unresolved legacy publications were not detected")
	}
	changed, err := store.QuarantineLegacyPublications(quarantinedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 4 {
		t.Fatalf("quarantined %d records, want 4: %+v", len(changed), changed)
	}
	wantIDs := []string{"current-requested", "legacy-failed", "legacy-pushing", "legacy-requested"}
	for index, publication := range changed {
		if publication.ID != wantIDs[index] || publication.State != PublicationQuarantined || publication.OriginalState == "" || publication.QuarantinedAt != quarantinedAt || publication.QuarantineReason != LegacyPublicationQuarantineReason || publication.UpdatedAt != quarantinedAt {
			t.Fatalf("quarantined[%d] = %+v", index, publication)
		}
	}
	if store.HasUnquarantinedLegacyPublications() {
		t.Fatal("quarantined records still block readiness")
	}
	gotPublished, _ := store.Publication("published")
	gotAlready, _ := store.Publication("already")
	if !reflect.DeepEqual(gotPublished, published) || !reflect.DeepEqual(gotAlready, alreadyQuarantined) {
		t.Fatalf("terminal records changed: published=%+v already=%+v", gotPublished, gotAlready)
	}
	firstWrite, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := store.QuarantineLegacyPublications(quarantinedAt.Add(time.Hour)); err != nil || len(changed) != 0 {
		t.Fatalf("idempotent quarantine = %+v, %v", changed, err)
	}
	secondWrite, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(firstWrite, secondWrite) {
		t.Fatalf("idempotent quarantine rewrote state: err=%v", err)
	}
	reopened, err := Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := reopened.Publication("current-requested")
	if current.SchemaVersion != PublicationSchemaVersion || current.State != PublicationQuarantined || current.OriginalState != PublicationRequested {
		t.Fatalf("reopened current record = %+v", current)
	}
}

func TestQuarantineLegacyPublicationsRejectsMissingTimestampWithoutMutation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.QuarantineLegacyPublications(time.Time{}); err == nil {
		t.Fatal("quarantine accepted a zero timestamp")
	}
}

func testPreparedPublication(branch string) PreparedPublication {
	return PreparedPublication{
		RepositoryID: 123, RepositoryFullName: "owner/repo",
		BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BaseRef: "main",
		ResultCommit: "0123456789012345678901234567890123456789", Branch: branch,
	}
}

func testPullRequestObservation(prepared PreparedPublication) PullRequestObservation {
	owner, name := "owner", "repo"
	return PullRequestObservation{
		TargetRepositoryID: prepared.RepositoryID, TargetRepositoryFullName: prepared.RepositoryFullName,
		Number: 1, URL: "https://github.com/owner/repo/pull/1", State: "open", Draft: true,
		Base: PullRequestRefObservation{RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, RepositoryOwner: owner, RepositoryName: name, Ref: prepared.BaseRef, SHA: prepared.BaseSHA},
		Head: PullRequestRefObservation{RepositoryID: prepared.RepositoryID, RepositoryFullName: prepared.RepositoryFullName, RepositoryOwner: owner, RepositoryName: name, Ref: prepared.Branch, SHA: prepared.ResultCommit},
	}
}
