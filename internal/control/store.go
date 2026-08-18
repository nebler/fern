package control

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
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

type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type WorkflowStatus string

const (
	WorkflowRecorded             WorkflowStatus = "recorded"
	WorkflowWorking              WorkflowStatus = "working"
	WorkflowWaitingForApproval   WorkflowStatus = "waiting_for_approval"
	WorkflowCompleted            WorkflowStatus = "completed"
	WorkflowPublicationRequested WorkflowStatus = "publication_requested"
	WorkflowPublished            WorkflowStatus = "published"
	WorkflowFailed               WorkflowStatus = "failed"
)

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

type Publication struct {
	ID         string    `json:"id"`
	WorkflowID string    `json:"workflowId"`
	State      string    `json:"state"`
	Operation  string    `json:"operation"`
	Base       string    `json:"base,omitempty"`
	Title      string    `json:"title"`
	Body       string    `json:"body,omitempty"`
	Repository string    `json:"repository,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	PullURL    string    `json:"pullUrl,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

const (
	PublicationRequested = "requested"
	PublicationPrepared  = "pushing"
	PublicationFailed    = "failed"
	PublicationPublished = "published"
)

type diskState struct {
	Version      int                    `json:"version"`
	Workspace    string                 `json:"workspace"`
	Revision     uint64                 `json:"revision"`
	Devices      map[string]Device      `json:"devices"`
	Workflows    map[string]Workflow    `json:"workflows"`
	Publications map[string]Publication `json:"publications"`
}

type Store struct {
	mu        sync.Mutex
	path      string
	workspace string
	data      diskState
}

func Open(directory, workspace string) (*Store, error) {
	if workspace == "" {
		return nil, errors.New("workspace is required for control store")
	}
	if err := ensureDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, fmt.Sprintf("%x.json", sha256.Sum256([]byte(workspace))))
	store := &Store{path: path, workspace: workspace, data: emptyState(workspace)}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

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
		for hash, expired := range pruned {
			store.data.Devices[hash] = expired
		}
		return Device{}, errors.New("device limit reached; revoke an existing device")
	}
	previous, existed := store.data.Devices[hash]
	store.data.Devices[hash] = device
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			for hash, expired := range pruned {
				store.data.Devices[hash] = expired
			}
			if existed {
				store.data.Devices[hash] = previous
			} else {
				delete(store.data.Devices, hash)
			}
		}
		return Device{}, err
	}
	return device, nil
}

func (store *Store) AuthenticateDevice(token string, now time.Time) (bool, error) {
	if token == "" {
		return false, nil
	}
	hash := tokenHash(token)
	store.mu.Lock()
	defer store.mu.Unlock()
	device, exists := store.data.Devices[hash]
	if !exists {
		return false, nil
	}
	if !now.Before(device.ExpiresAt) {
		delete(store.data.Devices, hash)
		if err := store.writeLocked(); err != nil {
			if rollbackWrite(err) {
				store.data.Devices[hash] = device
			}
			return false, err
		}
		return false, nil
	}
	if now.Sub(device.LastSeen) >= time.Hour {
		previous := device
		device.LastSeen = now.UTC()
		store.data.Devices[hash] = device
		if err := store.writeLocked(); err != nil {
			if rollbackWrite(err) {
				store.data.Devices[hash] = previous
			}
			return false, err
		}
	}
	return true, nil
}

func (store *Store) Devices(now time.Time) ([]Device, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	pruned := store.pruneLocked(now)
	if len(pruned) != 0 {
		if err := store.writeLocked(); err != nil {
			if rollbackWrite(err) {
				for hash, device := range pruned {
					store.data.Devices[hash] = device
				}
			}
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
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			for hash, device := range removed {
				store.data.Devices[hash] = device
			}
		}
		return err
	}
	return nil
}

func (store *Store) CreateWorkflow(title, sessionID string, now time.Time) (Workflow, error) {
	title, sessionID = strings.TrimSpace(title), strings.TrimSpace(sessionID)
	if title == "" || len(title) > 200 || sessionID == "" || len(sessionID) > 200 {
		return Workflow{}, errors.New("workflow title and OpenCode session ID are required and bounded")
	}
	id, err := randomID()
	if err != nil {
		return Workflow{}, err
	}
	workflow := Workflow{ID: id, Title: title, SessionID: sessionID, Status: WorkflowWorking, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, existing := range store.data.Workflows {
		if existing.SessionID == sessionID {
			if existing.Title != title {
				return Workflow{}, errors.New("OpenCode session is already tracked with a different title")
			}
			return existing, nil
		}
	}
	if len(store.data.Workflows) >= 256 {
		return Workflow{}, errors.New("workflow limit reached")
	}
	workflow.Status = WorkflowRecorded
	workflow.Revision = 1
	store.data.Workflows[id] = workflow
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			delete(store.data.Workflows, id)
		}
		return Workflow{}, err
	}
	return workflow, nil
}

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

func (store *Store) Workflow(id string) (Workflow, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	workflow, exists := store.data.Workflows[id]
	return workflow, exists
}

func (store *Store) UpdateWorkflow(id string, status WorkflowStatus, publicationID string, now time.Time) (Workflow, error) {
	if !validWorkflowStatus(status) {
		return Workflow{}, fmt.Errorf("invalid workflow status %q", status)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	workflow, exists := store.data.Workflows[id]
	if !exists {
		return Workflow{}, os.ErrNotExist
	}
	previous := workflow
	workflow.Status = status
	workflow.PublicationID = publicationID
	workflow.UpdatedAt = now.UTC()
	workflow.Revision++
	store.data.Workflows[id] = workflow
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			store.data.Workflows[id] = previous
		}
		return Workflow{}, err
	}
	return workflow, nil
}

func (store *Store) PutPublication(publication Publication) error {
	if publication.ID == "" || publication.WorkflowID == "" || publication.State == "" {
		return errors.New("publication ID, workflow ID, and state are required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	previous, existed := store.data.Publications[publication.ID]
	store.data.Publications[publication.ID] = publication
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			if existed {
				store.data.Publications[publication.ID] = previous
			} else {
				delete(store.data.Publications, publication.ID)
			}
		}
		return err
	}
	return nil
}

func (store *Store) RequestPublication(workflowID string, publication Publication, now time.Time) (Publication, bool, error) {
	if publication.ID == "" || publication.Operation == "" || strings.TrimSpace(publication.Title) == "" {
		return Publication{}, false, errors.New("publication ID, operation, and title are required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	workflow, exists := store.data.Workflows[workflowID]
	if !exists {
		return Publication{}, false, os.ErrNotExist
	}
	if workflow.PublicationID != "" {
		existing, exists := store.data.Publications[workflow.PublicationID]
		if !exists {
			return Publication{}, false, errors.New("workflow references a missing publication")
		}
		return existing, false, nil
	}
	if len(store.data.Publications) >= 256 {
		return Publication{}, false, errors.New("publication limit reached")
	}
	previousWorkflow := workflow
	publication.WorkflowID = workflowID
	publication.State = "requested"
	publication.CreatedAt = now.UTC()
	publication.UpdatedAt = now.UTC()
	workflow.Status = WorkflowPublicationRequested
	workflow.PublicationID = publication.ID
	workflow.UpdatedAt = now.UTC()
	workflow.Revision++
	store.data.Publications[publication.ID] = publication
	store.data.Workflows[workflowID] = workflow
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			delete(store.data.Publications, publication.ID)
			store.data.Workflows[workflowID] = previousWorkflow
		}
		return Publication{}, false, err
	}
	return publication, true, nil
}

func (store *Store) PreparePublication(id, repository, base, branch, commit string, now time.Time) error {
	if repository == "" || base == "" || branch == "" || commit == "" {
		return errors.New("prepared publication repository, base, branch, and commit are required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	publication, exists := store.data.Publications[id]
	if !exists {
		return os.ErrNotExist
	}
	if publication.Commit != "" && (publication.Commit != commit || publication.Branch != branch || publication.Repository != repository || publication.Base != base) {
		return errors.New("publication retry resolved to different repository state")
	}
	if publication.State == "published" {
		return nil
	}
	previous := publication
	publication.State = "pushing"
	publication.Repository = repository
	publication.Base = base
	publication.Branch = branch
	publication.Commit = commit
	publication.Error = ""
	publication.UpdatedAt = now.UTC()
	store.data.Publications[id] = publication
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			store.data.Publications[id] = previous
		}
		return err
	}
	return nil
}

func (store *Store) FinishPublication(id, pullURL, failure string, now time.Time) (Publication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	publication, exists := store.data.Publications[id]
	if !exists {
		return Publication{}, os.ErrNotExist
	}
	if publication.State == "published" {
		return publication, nil
	}
	workflow, exists := store.data.Workflows[publication.WorkflowID]
	if !exists {
		return Publication{}, errors.New("publication references a missing workflow")
	}
	previousPublication, previousWorkflow := publication, workflow
	publication.PullURL = pullURL
	publication.Error = failure
	publication.UpdatedAt = now.UTC()
	workflow.UpdatedAt = now.UTC()
	workflow.Revision++
	if failure == "" {
		publication.State = "published"
		workflow.Status = WorkflowPublished
	} else {
		publication.State = "failed"
		workflow.Status = WorkflowFailed
	}
	store.data.Publications[id] = publication
	store.data.Workflows[workflow.ID] = workflow
	if err := store.writeLocked(); err != nil {
		if rollbackWrite(err) {
			store.data.Publications[id] = previousPublication
			store.data.Workflows[workflow.ID] = previousWorkflow
		}
		return Publication{}, err
	}
	return publication, nil
}

func (store *Store) Publication(id string) (Publication, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	publication, exists := store.data.Publications[id]
	return publication, exists
}

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

func validWorkflowStatus(status WorkflowStatus) bool {
	switch status {
	case WorkflowRecorded, WorkflowWorking, WorkflowWaitingForApproval, WorkflowCompleted, WorkflowPublicationRequested, WorkflowPublished, WorkflowFailed:
		return true
	default:
		return false
	}
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
	initializeMaps(&state)
	store.data = state
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

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
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
