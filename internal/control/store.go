package control

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const maxDevices = 64

// Device is a paired browser credential. Only the SHA-256 hash of the bearer
// token is ever stored; ID is that hash's leading bytes.
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Store is the workspace's control-plane identity state in Fern's SQLite
// database. mu guards only the in-memory request registry and its fence
// against revocation.
type Store struct {
	db                   *sql.DB
	mu                   sync.Mutex
	activeDeviceRequests map[string]map[uint64]func()
	nextDeviceRequestID  uint64
}

// New returns the control store backed by db, whose schema (taskstore) defines
// the devices and operator_credential tables.
func New(db *sql.DB) *Store {
	return &Store{db: db, activeDeviceRequests: make(map[string]map[uint64]func())}
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
	tx, err := store.db.Begin()
	if err != nil {
		return Device{}, fmt.Errorf("begin Fern control transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM devices WHERE expires_at<=?`, now.UnixNano()); err != nil {
		return Device{}, fmt.Errorf("prune Fern devices: %w", err)
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM devices`).Scan(&count); err != nil {
		return Device{}, fmt.Errorf("count Fern devices: %w", err)
	}
	if count >= maxDevices {
		return Device{}, errors.New("device limit reached; revoke an existing device")
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO devices(token_sha256,name,created_at,last_seen,expires_at) VALUES(?,?,?,?,?)`,
		hash, device.Name, device.CreatedAt.UnixNano(), device.LastSeen.UnixNano(), device.ExpiresAt.UnixNano()); err != nil {
		return Device{}, fmt.Errorf("record Fern device: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Device{}, fmt.Errorf("commit Fern device: %w", err)
	}
	return device, nil
}

// AuthenticateDeviceIdentity validates a device bearer token and returns its
// durable identity, deleting the matched expired credential and refreshing
// LastSeen at most once an hour along the way.
func (store *Store) AuthenticateDeviceIdentity(token string, now time.Time) (Device, bool, error) {
	if token == "" {
		return Device{}, false, nil
	}
	hash := tokenHash(token)
	device, err := scanDevice(store.db.QueryRow(`SELECT `+deviceColumns+` FROM devices WHERE token_sha256=?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, false, nil
	}
	if err != nil {
		return Device{}, false, fmt.Errorf("read Fern device: %w", err)
	}
	if !now.Before(device.ExpiresAt) {
		if _, err := store.db.Exec(`DELETE FROM devices WHERE token_sha256=?`, hash); err != nil {
			return Device{}, false, fmt.Errorf("delete expired Fern device: %w", err)
		}
		return Device{}, false, nil
	}
	if now.Sub(device.LastSeen) >= time.Hour {
		device.LastSeen = now.UTC()
		if _, err := store.db.Exec(`UPDATE devices SET last_seen=? WHERE token_sha256=?`, device.LastSeen.UnixNano(), hash); err != nil {
			return Device{}, false, fmt.Errorf("refresh Fern device: %w", err)
		}
	}
	return device, true, nil
}

// RegisterDeviceRequest fences admission against durable revocation. The
// returned cleanup must be called when the admitted request completes.
func (store *Store) RegisterDeviceRequest(deviceID string, cancel func()) (func(), bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var found bool
	if err := store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM devices WHERE substr(token_sha256,1,16)=?)`, deviceID).Scan(&found); err != nil || !found {
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
// been persisted.
func (store *Store) CancelDeviceRequests(deviceID string) {
	store.mu.Lock()
	requests := store.activeDeviceRequests[deviceID]
	delete(store.activeDeviceRequests, deviceID)
	store.mu.Unlock()
	for _, cancel := range requests {
		cancel()
	}
}

// Devices lists every unexpired device oldest-first, durably deleting expired
// entries.
func (store *Store) Devices(now time.Time) ([]Device, error) {
	if _, err := store.db.Exec(`DELETE FROM devices WHERE expires_at<=?`, now.UnixNano()); err != nil {
		return nil, fmt.Errorf("prune Fern devices: %w", err)
	}
	rows, err := store.db.Query(`SELECT ` + deviceColumns + ` FROM devices ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list Fern devices: %w", err)
	}
	defer rows.Close()
	result := []Device{}
	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("list Fern devices: %w", err)
		}
		result = append(result, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Fern devices: %w", err)
	}
	return result, nil
}

// RevokeDevice durably removes every credential sharing the device ID.
// Callers then call CancelDeviceRequests.
func (store *Store) RevokeDevice(id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	result, err := store.db.Exec(`DELETE FROM devices WHERE substr(token_sha256,1,16)=?`, id)
	if err != nil {
		return fmt.Errorf("revoke Fern device: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke Fern device: %w", err)
	}
	if removed == 0 {
		return os.ErrNotExist
	}
	return nil
}

const deviceColumns = `substr(token_sha256,1,16),name,created_at,last_seen,expires_at`

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var device Device
	var createdAt, lastSeen, expiresAt int64
	if err := row.Scan(&device.ID, &device.Name, &createdAt, &lastSeen, &expiresAt); err != nil {
		return Device{}, err
	}
	device.CreatedAt = time.Unix(0, createdAt).UTC()
	device.LastSeen = time.Unix(0, lastSeen).UTC()
	device.ExpiresAt = time.Unix(0, expiresAt).UTC()
	return device, nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// OperatorCredentialIDPrefix begins every operator credential identifier so
// audit snapshots can distinguish control-surface credentials from device IDs.
const OperatorCredentialIDPrefix = "control-"

// newOperatorCredentialID mints a fresh random operator credential identifier.
// It carries no derived secret material, so persisting it in audit snapshots
// creates no offline guessing opportunity.
func newOperatorCredentialID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate Fern operator credential ID: %w", err)
	}
	return OperatorCredentialIDPrefix + base64.RawURLEncoding.EncodeToString(random), nil
}

// EnsureOperatorCredentialID returns the stable random identifier attributed to
// control-password operators in audit snapshots, generating and durably
// recording one on first use. The identifier is pure randomness — never a
// derivation of the control password — so durable audit records cannot become
// an offline brute-force oracle for that secret. It is an identifier, not a
// secret.
func (store *Store) EnsureOperatorCredentialID() (string, error) {
	generated, err := newOperatorCredentialID()
	if err != nil {
		return "", err
	}
	if _, err := store.db.Exec(`INSERT INTO operator_credential(singleton,id) VALUES(1,?) ON CONFLICT DO NOTHING`, generated); err != nil {
		return "", fmt.Errorf("record Fern operator credential ID: %w", err)
	}
	var id string
	if err := store.db.QueryRow(`SELECT id FROM operator_credential`).Scan(&id); err != nil {
		return "", fmt.Errorf("read Fern operator credential ID: %w", err)
	}
	return id, nil
}
