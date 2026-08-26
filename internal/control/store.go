package control

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const schemaVersion = 1
const maxControlStateBytes = 4 << 20

// PublicationSchemaVersion is the durable-proof schema version required for
// any publication record still allowed to change.
const PublicationSchemaVersion = 1

// Device is a paired browser credential. Only the SHA-256 hash of the bearer
// token is ever stored; ID is that hash's leading bytes.
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// WorkflowStatus enumerates the durable lifecycle states of a tracked workflow.
type WorkflowStatus string

// Workflow lifecycle statuses persisted on every workflow record.
const (
	WorkflowRecorded             WorkflowStatus = "recorded"
	WorkflowWorking              WorkflowStatus = "working"
	WorkflowWaitingForApproval   WorkflowStatus = "waiting_for_approval"
	WorkflowCompleted            WorkflowStatus = "completed"
	WorkflowPublicationRequested WorkflowStatus = "publication_requested"
	WorkflowPublished            WorkflowStatus = "published"
	WorkflowFailed               WorkflowStatus = "failed"
)

// Workflow is an OpenCode session Fern tracks, with its publication linkage.
type Workflow struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	SessionID     string         `json:"sessionId"`
	Status        WorkflowStatus `json:"status"`
	PublicationID string         `json:"publicationId,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	Revision      uint64         `json:"revision"`
}

// Publication is read-only audit evidence from the retired control-plane
// publication path. Only explicit offline quarantine metadata may be added.
type Publication struct {
	SchemaVersion      int                     `json:"schemaVersion,omitempty"`
	ID                 string                  `json:"id"`
	WorkflowID         string                  `json:"workflowId"`
	State              string                  `json:"state"`
	Operation          string                  `json:"operation"`
	RequestedBaseRef   string                  `json:"requestedBaseRef,omitempty"`
	Title              string                  `json:"title"`
	Body               string                  `json:"body,omitempty"`
	RepositoryID       int64                   `json:"repositoryId,omitempty"`
	RepositoryFullName string                  `json:"repositoryFullName,omitempty"`
	BaseSHA            string                  `json:"baseSha,omitempty"`
	BaseRef            string                  `json:"baseRef,omitempty"`
	ResultCommit       string                  `json:"resultCommit,omitempty"`
	Branch             string                  `json:"branch,omitempty"`
	PullRequest        *PullRequestObservation `json:"pullRequest,omitempty"`
	// Legacy fields are retained so old terminal records remain audit-readable.
	Base             string    `json:"base,omitempty"`
	Repository       string    `json:"repository,omitempty"`
	Commit           string    `json:"commit,omitempty"`
	PullURL          string    `json:"pullUrl,omitempty"`
	Error            string    `json:"error,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	OriginalState    string    `json:"originalState,omitempty"`
	QuarantinedAt    time.Time `json:"quarantinedAt,omitempty"`
	QuarantineReason string    `json:"quarantineReason,omitempty"`
}

// PreparedPublication describes the resolved tuple validated while loading
// versioned audit records.
type PreparedPublication struct {
	RepositoryID       int64  `json:"repositoryId"`
	RepositoryFullName string `json:"repositoryFullName"`
	BaseSHA            string `json:"baseSha"`
	BaseRef            string `json:"baseRef"`
	ResultCommit       string `json:"resultCommit"`
	Branch             string `json:"branch"`
}

// PullRequestRefObservation is the observed state of one ref (base or head) of
// a published pull request.
type PullRequestRefObservation struct {
	RepositoryID       int64  `json:"repositoryId"`
	RepositoryFullName string `json:"repositoryFullName"`
	RepositoryOwner    string `json:"repositoryOwner"`
	RepositoryName     string `json:"repositoryName"`
	Ref                string `json:"ref"`
	SHA                string `json:"sha"`
}

// PullRequestObservation is the complete durable proof that a publication
// produced one specific draft pull request.
type PullRequestObservation struct {
	TargetRepositoryID       int64                     `json:"targetRepositoryId"`
	TargetRepositoryFullName string                    `json:"targetRepositoryFullName"`
	Number                   int64                     `json:"number"`
	URL                      string                    `json:"url"`
	State                    string                    `json:"state"`
	Draft                    bool                      `json:"draft"`
	Base                     PullRequestRefObservation `json:"base"`
	Head                     PullRequestRefObservation `json:"head"`
}

// Publication lifecycle states persisted on every publication record.
const (
	PublicationRequested   = "requested"
	PublicationPrepared    = "pushing"
	PublicationFailed      = "failed"
	PublicationPublished   = "published"
	PublicationQuarantined = "quarantined"

	LegacyPublicationQuarantineReason = "legacy control publication retired; external effects were not resumed"
)

// diskState is the durable control file. The strict loader treats absent
// optional fields as their zero value, so new fields must tolerate being empty
// on load instead of forcing a format bump.
type diskState struct {
	Version              int                    `json:"version"`
	Workspace            string                 `json:"workspace"`
	Revision             uint64                 `json:"revision"`
	OperatorCredentialID string                 `json:"operatorCredentialId,omitempty"`
	Devices              map[string]Device      `json:"devices"`
	Workflows            map[string]Workflow    `json:"workflows"`
	Publications         map[string]Publication `json:"publications"`
}

// Store is the durable control-plane state for one workspace: paired devices,
// tracked workflows, and publication records, guarded by a mutex and an atomic
// private-file write path.
type Store struct {
	mu                   sync.Mutex
	path                 string
	workspace            string
	data                 diskState
	activeDeviceRequests map[string]map[uint64]func()
	nextDeviceRequestID  uint64
}

// Open loads (or initializes) the control state for workspace inside directory.
// The directory and its state file must satisfy the private-file rules or Open
// refuses to run.
func Open(directory, workspace string) (*Store, error) {
	if workspace == "" {
		return nil, errors.New("workspace is required for control store")
	}
	if err := ensureDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(workspace))))
	store := &Store{
		path:                 path,
		workspace:            workspace,
		data:                 emptyState(workspace),
		activeDeviceRequests: make(map[string]map[uint64]func()),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// AuxiliaryStatePath returns a sibling path for a small subsystem-owned state
// file. The closed name prevents callers from escaping the control directory.
func (store *Store) AuxiliaryStatePath(name string) (string, error) {
	if store == nil || store.path == "" {
		return "", errors.New("control store is unavailable")
	}
	if name == "" || len(name) > 32 {
		return "", errors.New("invalid auxiliary state name")
	}
	for _, character := range name {
		if character < 'a' || character > 'z' {
			return "", errors.New("invalid auxiliary state name")
		}
	}
	return store.path + "." + name, nil
}

// AddDevice durably registers a paired browser credential, pruning expired
// devices before admitting against the 64-device cap. The raw token is never
// persisted.
func (store *Store) AddDevice(token, name string, now, expires time.Time) (Device, error) {
	if token == "" || !expires.After(now) {
		return Device{}, errors.New("valid device token and expiry are required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Paired browser"
	}
	if len(name) > 80 {
		return Device{}, errors.New("device name exceeds 80 bytes")
	}
	hash := tokenHash(token)
	device := Device{ID: hash[:16], Name: name, CreatedAt: now.UTC(), LastSeen: now.UTC(), ExpiresAt: expires.UTC()}
	store.mu.Lock()
	defer store.mu.Unlock()
	pruned := store.pruneLocked(now)
	if len(store.data.Devices) >= 64 {
		// No durable write follows this rejection, so memory must keep matching
		// disk by resurrecting everything pruning removed.
		store.restorePrunedLocked(pruned)
		return Device{}, errors.New("device limit reached; revoke an existing device")
	}
	previous, existed := store.data.Devices[hash]
	store.data.Devices[hash] = device
	err := store.commitLocked(func() {
		store.restorePrunedLocked(pruned)
		if existed {
			store.data.Devices[hash] = previous
		} else {
			delete(store.data.Devices, hash)
		}
	})
	if err != nil {
		return Device{}, err
	}
	return device, nil
}

// AuthenticateDeviceIdentity validates a device bearer token and returns its
// durable identity, pruning expired credentials and refreshing LastSeen at most
// once an hour along the way.
func (store *Store) AuthenticateDeviceIdentity(token string, now time.Time) (Device, bool, error) {
	if token == "" {
		return Device{}, false, nil
	}
	hash := tokenHash(token)
	store.mu.Lock()
	defer store.mu.Unlock()
	device, exists := store.data.Devices[hash]
	if !exists {
		return Device{}, false, nil
	}
	if !now.Before(device.ExpiresAt) {
		delete(store.data.Devices, hash)
		if err := store.commitLocked(func() { store.data.Devices[hash] = device }); err != nil {
			return Device{}, false, err
		}
		return Device{}, false, nil
	}
	if now.Sub(device.LastSeen) >= time.Hour {
		previous := device
		device.LastSeen = now.UTC()
		store.data.Devices[hash] = device
		if err := store.commitLocked(func() { store.data.Devices[hash] = previous }); err != nil {
			return Device{}, false, err
		}
	}
	return device, true, nil
}

// RegisterDeviceRequest fences admission against durable revocation. The
// returned cleanup must be called when the admitted request completes.
func (store *Store) RegisterDeviceRequest(deviceID string, cancel func()) (func(), bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	found := false
	for _, device := range store.data.Devices {
		if device.ID == deviceID {
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}
	store.nextDeviceRequestID++
	requestID := store.nextDeviceRequestID
	requests := store.activeDeviceRequests[deviceID]
	if requests == nil {
		requests = make(map[uint64]func())
		store.activeDeviceRequests[deviceID] = requests
	}
	requests[requestID] = cancel
	return func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		requests := store.activeDeviceRequests[deviceID]
		delete(requests, requestID)
		if len(requests) == 0 {
			delete(store.activeDeviceRequests, deviceID)
		}
	}, true
}

// CancelDeviceRequests is the in-memory callback run only after revocation has
// been persisted. Cancellation callbacks are deliberately absent from diskState.
func (store *Store) CancelDeviceRequests(deviceID string) {
	store.mu.Lock()
	requests := store.activeDeviceRequests[deviceID]
	delete(store.activeDeviceRequests, deviceID)
	store.mu.Unlock()
	for _, cancel := range requests {
		cancel()
	}
}

// Devices lists every unexpired device oldest-first, durably persisting the
// pruning of expired entries.
func (store *Store) Devices(now time.Time) ([]Device, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	pruned := store.pruneLocked(now)
	if len(pruned) != 0 {
		if err := store.commitLocked(func() { store.restorePrunedLocked(pruned) }); err != nil {
			return nil, err
		}
	}
	result := make([]Device, 0, len(store.data.Devices))
	for _, device := range store.data.Devices {
		result = append(result, device)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

// RevokeDevice durably removes every credential sharing the device ID.
func (store *Store) RevokeDevice(id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	found := false
	removed := make(map[string]Device)
	for hash, device := range store.data.Devices {
		if device.ID == id {
			removed[hash] = device
			delete(store.data.Devices, hash)
			found = true
		}
	}
	if !found {
		return os.ErrNotExist
	}
	return store.commitLocked(func() {
		for hash, device := range removed {
			store.data.Devices[hash] = device
		}
	})
}

// Workflows snapshots the retired workflow audit records, most recently
// updated first. No active control route exposes them.
func (store *Store) Workflows() []Workflow {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]Workflow, 0, len(store.data.Workflows))
	for _, workflow := range store.data.Workflows {
		result = append(result, workflow)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result
}

// Workflow returns the tracked workflow with the given ID.
func (store *Store) Workflow(id string) (Workflow, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	workflow, exists := store.data.Workflows[id]
	return workflow, exists
}

func preparedFromPublication(publication Publication) PreparedPublication {
	return PreparedPublication{
		RepositoryID: publication.RepositoryID, RepositoryFullName: publication.RepositoryFullName,
		BaseSHA: publication.BaseSHA, BaseRef: publication.BaseRef,
		ResultCommit: publication.ResultCommit, Branch: publication.Branch,
	}
}

func validatePreparedPublication(prepared PreparedPublication) error {
	if prepared.RepositoryID <= 0 || !validRepositoryFullName(prepared.RepositoryFullName) || !validGitRef(prepared.BaseRef) || !validGitRef(prepared.Branch) || prepared.BaseRef == prepared.Branch || !validSHA(prepared.BaseSHA) || !validSHA(prepared.ResultCommit) {
		return errors.New("complete prepared publication tuple is required")
	}
	return nil
}

func validPullRequestObservation(observation PullRequestObservation, prepared PreparedPublication) bool {
	owner, name, ok := splitRepositoryFullName(prepared.RepositoryFullName)
	if !ok || observation.TargetRepositoryID != prepared.RepositoryID || observation.TargetRepositoryFullName != prepared.RepositoryFullName || observation.Number <= 0 || observation.State != "open" || !observation.Draft {
		return false
	}
	wantURL := "https://github.com/" + prepared.RepositoryFullName + "/pull/" + fmt.Sprintf("%d", observation.Number)
	if observation.URL != wantURL {
		return false
	}
	return validPullRequestRef(observation.Base, prepared.RepositoryID, prepared.RepositoryFullName, owner, name, prepared.BaseRef, prepared.BaseSHA) &&
		validPullRequestRef(observation.Head, prepared.RepositoryID, prepared.RepositoryFullName, owner, name, prepared.Branch, prepared.ResultCommit)
}

func validPullRequestRef(observation PullRequestRefObservation, repositoryID int64, fullName, owner, name, ref, sha string) bool {
	return observation.RepositoryID == repositoryID && observation.RepositoryFullName == fullName && observation.RepositoryOwner == owner && observation.RepositoryName == name && observation.Ref == ref && observation.SHA == sha
}

func splitRepositoryFullName(fullName string) (string, string, bool) {
	if strings.Count(fullName, "/") != 1 {
		return "", "", false
	}
	owner, name, _ := strings.Cut(fullName, "/")
	return owner, name, owner != "" && name != ""
}

func validRepositoryFullName(fullName string) bool {
	if len(fullName) < 3 || len(fullName) > 140 || strings.Count(fullName, "/") != 1 {
		return false
	}
	owner, name, _ := strings.Cut(fullName, "/")
	if len(owner) == 0 || len(owner) > 39 || owner[0] == '-' || owner[len(owner)-1] == '-' || len(name) == 0 || len(name) > 100 || name == "." || name == ".." || strings.HasSuffix(strings.ToLower(name), ".git") {
		return false
	}
	for index := range len(owner) {
		character := owner[index]
		if !asciiAlphaNumeric(character) && character != '-' {
			return false
		}
	}
	for index := range len(name) {
		character := name[index]
		if !asciiAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func validGitRef(value string) bool {
	if value == "" || value == "@" || len(value) > 255 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, ".") || strings.HasSuffix(value, "/") || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func validSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

// Publication returns the publication record with the given ID.
func (store *Store) Publication(id string) (Publication, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	publication, exists := store.data.Publications[id]
	return publication, exists
}

// Publications snapshots every publication record, most recently updated
// first.
func (store *Store) Publications() []Publication {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]Publication, 0, len(store.data.Publications))
	for _, publication := range store.data.Publications {
		result = append(result, publication)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result
}

// HasUnquarantinedLegacyPublications reports whether retired control-plane
// publication work still needs an explicit offline operator decision.
func (store *Store) HasUnquarantinedLegacyPublications() bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, publication := range store.data.Publications {
		if unresolvedLegacyPublication(publication) {
			return true
		}
	}
	return false
}

// QuarantineLegacyPublications atomically retires unresolved control-plane
// publication records without attempting or inferring any external effect.
// Published records are audit evidence and are never changed. Repeated calls
// return no records and do not rewrite the state file.
func (store *Store) QuarantineLegacyPublications(now time.Time) ([]Publication, error) {
	if now.IsZero() {
		return nil, errors.New("quarantine time is required")
	}
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	originals := make(map[string]Publication)
	quarantined := make([]Publication, 0)
	for id, publication := range store.data.Publications {
		if !unresolvedLegacyPublication(publication) {
			continue
		}
		originals[id] = publication
		publication.OriginalState = publication.State
		publication.State = PublicationQuarantined
		publication.QuarantinedAt = now
		publication.QuarantineReason = LegacyPublicationQuarantineReason
		publication.UpdatedAt = now
		store.data.Publications[id] = publication
		quarantined = append(quarantined, publication)
	}
	if len(quarantined) == 0 {
		return nil, nil
	}
	if err := store.commitLocked(func() {
		for id, publication := range originals {
			store.data.Publications[id] = publication
		}
	}); err != nil {
		return nil, err
	}
	sort.Slice(quarantined, func(i, j int) bool { return quarantined[i].ID < quarantined[j].ID })
	return quarantined, nil
}

func unresolvedLegacyPublication(publication Publication) bool {
	return publication.State == PublicationRequested || publication.State == PublicationPrepared || publication.State == PublicationFailed
}

func (store *Store) load() error {
	file, err := os.OpenFile(store.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open Fern control state: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect Fern control state: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("Fern control state must be a private singly linked regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxControlStateBytes+1))
	if err != nil {
		return fmt.Errorf("read Fern control state: %w", err)
	}
	if len(data) > maxControlStateBytes {
		return errors.New("Fern control state exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state diskState
	if err := decoder.Decode(&state); err != nil {
		return fmt.Errorf("decode Fern control state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode Fern control state: trailing data")
	}
	if state.Version != schemaVersion {
		return fmt.Errorf("unsupported Fern control state version %d", state.Version)
	}
	if state.Workspace != store.workspace {
		return fmt.Errorf("Fern control state belongs to workspace %q", state.Workspace)
	}
	if !validOperatorCredentialID(state.OperatorCredentialID) {
		return errors.New("Fern control state has an invalid operator credential identifier")
	}
	initializeMaps(&state)
	for id, publication := range state.Publications {
		if publication.SchemaVersion == 0 {
			continue
		}
		if publication.SchemaVersion != PublicationSchemaVersion || !validCurrentPublication(publication) {
			return fmt.Errorf("publication %q has invalid durable proof", id)
		}
	}
	store.data = state
	return nil
}

func validCurrentPublication(publication Publication) bool {
	if publication.ID == "" || publication.WorkflowID == "" || publication.Operation == "" || strings.TrimSpace(publication.Title) == "" {
		return false
	}
	switch publication.State {
	case PublicationRequested, PublicationPrepared, PublicationFailed, PublicationPublished:
		if publication.OriginalState != "" || !publication.QuarantinedAt.IsZero() || publication.QuarantineReason != "" {
			return false
		}
	case PublicationQuarantined:
		if publication.OriginalState != PublicationRequested && publication.OriginalState != PublicationPrepared && publication.OriginalState != PublicationFailed {
			return false
		}
		if publication.QuarantinedAt.IsZero() || publication.QuarantineReason != LegacyPublicationQuarantineReason {
			return false
		}
	default:
		return false
	}
	prepared := preparedFromPublication(publication)
	hasPrepared := publication.ResultCommit != ""
	if hasPrepared != (publication.RepositoryID != 0 || publication.RepositoryFullName != "" || publication.BaseSHA != "" || publication.BaseRef != "" || publication.Branch != "") {
		return false
	}
	if hasPrepared && validatePreparedPublication(prepared) != nil {
		return false
	}
	effectiveState := publication.State
	if effectiveState == PublicationQuarantined {
		effectiveState = publication.OriginalState
	}
	if effectiveState == PublicationRequested && hasPrepared || effectiveState == PublicationPrepared && !hasPrepared {
		return false
	}
	if publication.PullRequest != nil {
		return publication.State == PublicationPublished && hasPrepared && publication.PullURL == publication.PullRequest.URL && validPullRequestObservation(*publication.PullRequest, prepared)
	}
	return publication.State != PublicationPublished && publication.PullURL == ""
}

// commitLocked persists the in-memory mutation made under a held store lock.
// On failure it invokes undo exactly when the durable outcome is known to still
// match disk (see rollbackWrite), restoring the pre-mutation values. An
// uncertain commit — the state file was replaced but the directory sync failed
// — must NOT roll back: disk may already hold the new state, so reverting
// memory would make memory diverge from disk.
func (store *Store) commitLocked(undo func()) error {
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) && undo != nil {
			undo()
		}
		return err
	}
	return nil
}

func (store *Store) writeLocked() error {
	previousRevision := store.data.Revision
	store.data.Revision++
	data, err := json.Marshal(store.data)
	if err != nil {
		store.data.Revision = previousRevision
		return fmt.Errorf("encode Fern control state: %w", err)
	}
	if len(data) > maxControlStateBytes {
		store.data.Revision = previousRevision
		return errors.New("Fern control state exceeds 4 MiB")
	}
	directory := filepath.Dir(store.path)
	temporary, err := os.CreateTemp(directory, ".control-*.tmp")
	if err != nil {
		store.data.Revision = previousRevision
		return fmt.Errorf("create temporary Fern control state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		store.data.Revision = previousRevision
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		store.data.Revision = previousRevision
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		store.data.Revision = previousRevision
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		store.data.Revision = previousRevision
		return err
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		store.data.Revision = previousRevision
		return fmt.Errorf("replace Fern control state: %w", err)
	}
	handle, err := os.Open(directory)
	if err != nil {
		return commitUncertainError{err}
	}
	if err := errors.Join(handle.Sync(), handle.Close()); err != nil {
		return commitUncertainError{err}
	}
	return nil
}

type commitUncertainError struct {
	err error
}

func (err commitUncertainError) Error() string {
	return "Fern control state was replaced but directory sync failed: " + err.err.Error()
}

func (err commitUncertainError) Unwrap() error {
	return err.err
}

func rollbackWrite(err error) bool {
	var uncertain commitUncertainError
	return !errors.As(err, &uncertain)
}

// restorePrunedLocked resurrects devices that pruning removed but that no
// successful write has persisted yet. Whenever a mutation fails before any
// durable write, memory must keep matching disk exactly, so pruned entries
// cannot simply vanish from the in-memory map.
func (store *Store) restorePrunedLocked(pruned map[string]Device) {
	for hash, device := range pruned {
		store.data.Devices[hash] = device
	}
}

func (store *Store) pruneLocked(now time.Time) map[string]Device {
	pruned := make(map[string]Device)
	for hash, device := range store.data.Devices {
		if !now.Before(device.ExpiresAt) {
			pruned[hash] = device
			delete(store.data.Devices, hash)
		}
	}
	return pruned
}

func emptyState(workspace string) diskState {
	state := diskState{Version: schemaVersion, Workspace: workspace}
	initializeMaps(&state)
	return state
}

func initializeMaps(state *diskState) {
	if state.Devices == nil {
		state.Devices = make(map[string]Device)
	}
	if state.Workflows == nil {
		state.Workflows = make(map[string]Workflow)
	}
	if state.Publications == nil {
		state.Publications = make(map[string]Publication)
	}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// OperatorCredentialIDPrefix begins every operator credential identifier so
// audit snapshots can distinguish control-surface credentials from device IDs.
const OperatorCredentialIDPrefix = "control-"

// NewOperatorCredentialID mints a fresh random operator credential identifier.
// It carries no derived secret material, so persisting it in audit snapshots
// creates no offline guessing opportunity.
func NewOperatorCredentialID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate Fern operator credential ID: %w", err)
	}
	return OperatorCredentialIDPrefix + base64.RawURLEncoding.EncodeToString(random), nil
}

// EnsureOperatorCredentialID returns the stable random identifier attributed to
// control-password operators in audit snapshots, generating and durably
// recording one on first use through the same atomic write path as every other
// mutation. The identifier is pure randomness — never a derivation of the
// control password — so durable audit records cannot become an offline
// brute-force oracle for that secret. It is an identifier, not a secret.
func (store *Store) EnsureOperatorCredentialID() (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id := store.data.OperatorCredentialID; id != "" {
		return id, nil
	}
	generated, err := NewOperatorCredentialID()
	if err != nil {
		return "", err
	}
	store.data.OperatorCredentialID = generated
	if err := store.commitLocked(func() { store.data.OperatorCredentialID = "" }); err != nil {
		return "", err
	}
	return generated, nil
}

// validOperatorCredentialID accepts either the empty value — state files
// written before the field existed load unchanged — or exactly the canonical
// spelling produced by NewOperatorCredentialID.
func validOperatorCredentialID(value string) bool {
	if value == "" {
		return true
	}
	suffix, ok := strings.CutPrefix(value, OperatorCredentialIDPrefix)
	if !ok || len(suffix) != base64.RawURLEncoding.EncodedLen(16) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(suffix)
	return err == nil && len(decoded) == 16 && base64.RawURLEncoding.EncodeToString(decoded) == suffix
}

func ensureDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Fern control directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("Fern control directory must be a private real directory")
	}
	return nil
}
