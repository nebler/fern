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

const schemaVersion = 2
const maxControlStateBytes = 4 << 20

// Device is a paired browser credential. Only the SHA-256 hash of the bearer
// token is ever stored; ID is that hash's leading bytes.
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// diskState is the current durable control file. Historical schemas are rejected
// without migration or mutation; unknown fields are not accepted.
type diskState struct {
	Version              int               `json:"version"`
	Workspace            string            `json:"workspace"`
	Revision             uint64            `json:"revision"`
	OperatorCredentialID string            `json:"operatorCredentialId,omitempty"`
	Devices              map[string]Device `json:"devices"`
}

// Store is the durable control-plane identity state for one workspace, guarded
// by a mutex and an atomic private-file write path.
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
// file. Restricted lowercase name syntax prevents escaping the control directory.
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
// durable identity, pruning the matched expired credential and refreshing LastSeen at most
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
	// Read the version before strict decoding so historical files report the
	// unsupported schema even when they contain removed fields.
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("decode Fern control state: %w", err)
	}
	if header.Version != schemaVersion {
		return fmt.Errorf("unsupported Fern control state version %d", header.Version)
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
	if state.Workspace != store.workspace {
		return fmt.Errorf("Fern control state belongs to workspace %q", state.Workspace)
	}
	if !validOperatorCredentialID(state.OperatorCredentialID) {
		return errors.New("Fern control state has an invalid operator credential identifier")
	}
	initializeMaps(&state)
	store.data = state
	return nil
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

// validOperatorCredentialID accepts either the empty value before the first
// operator credential is issued, or exactly the canonical
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
