// Package pluginauth owns the fixed-scope OpenCode plugin authorization state.
package pluginauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
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

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/jsoncanon"
	"github.com/nebler/fern/internal/task"
)

const (
	stateVersion       = 1
	maxStateBytes      = 256 << 10
	maxAuthorizations  = 64
	maxCredentials     = 32
	maxInvalidPolls    = 64
	authorizationTTL   = 10 * time.Minute
	credentialTTL      = 90 * 24 * time.Hour
	startInterval      = time.Second
	pollInterval       = 5 * time.Second
	failureWindow      = 5 * time.Minute
	terminalRetention  = 24 * time.Hour
	deviceCodeBytes    = 32
	randomIDBytes      = 16
	userCodeBytes      = 8
	authorizationIDTag = "pa_"
	credentialIDTag    = "pc_"
)

var fixedScopes = [...]string{"run:create", "run:read", "run:stop", "run:attach", "run:result"}

// Scopes returns the complete and non-configurable plugin authority.
func Scopes() []string {
	return append([]string(nil), fixedScopes[:]...)
}

type AuthorizationState string

const (
	Pending  AuthorizationState = "pending"
	Approved AuthorizationState = "approved"
	Denied   AuthorizationState = "denied"
	Expired  AuthorizationState = "expired"
)

type CredentialState string

const (
	Active            CredentialState = "active"
	Revoked           CredentialState = "revoked"
	CredentialExpired CredentialState = "expired"
)

var (
	ErrNotFound     = errors.New("plugin authorization not found")
	ErrInvalidCode  = errors.New("invalid plugin authorization code")
	ErrInvalidState = errors.New("invalid plugin authorization state")
	ErrRateLimited  = errors.New("plugin authorization rate limited")
	ErrCapacity     = errors.New("plugin authorization capacity reached")
)

// StartResult contains the two independent one-time protocol codes. Callers
// must not log it. Fern persists neither plaintext value.
type StartResult struct {
	AuthorizationID string
	DeviceCode      string
	UserCode        string
	ExpiresAt       time.Time
	Interval        time.Duration
}

type PollState string

const (
	PollPending  PollState = "pending"
	PollApproved PollState = "approved"
	PollDenied   PollState = "denied"
	PollExpired  PollState = "expired"
)

type PollResult struct {
	State        PollState
	CredentialID string
	ExpiresAt    time.Time
}

// Credential is the non-secret administrative projection of one plugin grant.
type Credential struct {
	ID              string          `json:"id"`
	AuthorizationID string          `json:"authorizationId"`
	State           CredentialState `json:"state"`
	CreatedAt       time.Time       `json:"createdAt"`
	ExpiresAt       time.Time       `json:"expiresAt"`
	RevokedAt       time.Time       `json:"revokedAt,omitempty"`
	ApprovedBy      Attribution     `json:"approvedBy"`
	RevokedBy       *Attribution    `json:"revokedBy,omitempty"`
}

// Attribution is immutable decision evidence. It contains identifiers only,
// never the authenticating secret or a secret-derived digest.
type Attribution struct {
	Type           task.ActorType `json:"type"`
	ID             string         `json:"id"`
	DisplayName    string         `json:"displayName,omitempty"`
	CredentialID   string         `json:"credentialId"`
	Authentication string         `json:"authentication"`
	RequestID      string         `json:"requestId"`
}

func AttributionFromActor(actor task.ActorSnapshot) (Attribution, error) {
	if err := actor.Validate(); err != nil {
		return Attribution{}, errors.New("valid plugin authorization actor is required")
	}
	return Attribution{actor.Type, actor.ID, actor.DisplayName, actor.CredentialID, actor.Authentication, actor.RequestID}, nil
}

func trustedAttributionFromActor(actor task.ActorSnapshot) (Attribution, error) {
	value, err := AttributionFromActor(actor)
	if err != nil || actor.Type != task.ActorDevice && actor.Type != task.ActorOperator {
		return Attribution{}, errors.New("trusted approval actor is required")
	}
	return value, nil
}

type authorizationRecord struct {
	ID           string             `json:"id"`
	DeviceDigest string             `json:"deviceDigest"`
	UserDigest   string             `json:"userDigest"`
	State        AuthorizationState `json:"state"`
	CreatedAt    time.Time          `json:"createdAt"`
	ExpiresAt    time.Time          `json:"expiresAt"`
	LastPolledAt time.Time          `json:"lastPolledAt,omitempty"`
	DecidedAt    time.Time          `json:"decidedAt,omitempty"`
	DecidedBy    *Attribution       `json:"decidedBy,omitempty"`
	CredentialID string             `json:"credentialId,omitempty"`
}

type credentialRecord struct {
	Credential
	DeviceDigest string `json:"deviceDigest"`
}

type diskState struct {
	Version        int                            `json:"version"`
	Workspace      string                         `json:"workspace"`
	Revision       uint64                         `json:"revision"`
	LastStartedAt  time.Time                      `json:"lastStartedAt,omitempty"`
	InvalidPolls   []time.Time                    `json:"invalidPolls,omitempty"`
	Authorizations map[string]authorizationRecord `json:"authorizations"`
	Credentials    map[string]credentialRecord    `json:"credentials"`
}

type Store struct {
	mu            sync.Mutex
	path          string
	workspace     string
	data          diskState
	active        map[string]map[uint64]context.CancelFunc
	nextRequestID uint64
}

type authorizationContextKey struct{}

// RequestAuthorization is installed only after bearer authentication and the
// authenticate/register race fence. Its scope set is always Scopes().
type RequestAuthorization struct {
	Credential Credential
}

func WithRequestAuthorization(ctx context.Context, credential Credential) context.Context {
	return context.WithValue(ctx, authorizationContextKey{}, RequestAuthorization{Credential: credential})
}

func RequestAuthorizationFromContext(ctx context.Context) (RequestAuthorization, bool) {
	value, ok := ctx.Value(authorizationContextKey{}).(RequestAuthorization)
	return value, ok
}

func (RequestAuthorization) HasScope(scope string) bool {
	for _, allowed := range fixedScopes {
		if scope == allowed {
			return true
		}
	}
	return false
}

// Open loads the subsystem-owned private auxiliary state. Missing state starts
// empty; malformed or unsafe existing state is a startup error.
func Open(controlStore *control.Store, workspace string) (*Store, error) {
	if controlStore == nil {
		return nil, errors.New("control store is required for plugin authorization")
	}
	if workspace == "" {
		return nil, errors.New("workspace is required for plugin authorization")
	}
	path, err := controlStore.AuxiliaryStatePath("pluginauth")
	if err != nil {
		return nil, err
	}
	store := &Store{path: path, workspace: workspace, data: emptyState(workspace), active: make(map[string]map[uint64]context.CancelFunc)}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *Store) Start(now time.Time) (StartResult, error) {
	now = now.UTC()
	deviceCode, err := randomBase64(deviceCodeBytes)
	if err != nil {
		return StartResult{}, err
	}
	userCode, err := randomUserCode()
	if err != nil {
		return StartResult{}, err
	}
	id, err := randomID(authorizationIDTag)
	if err != nil {
		return StartResult{}, err
	}
	record := authorizationRecord{
		ID: id, DeviceDigest: digest("device", deviceCode), UserDigest: digest("user", userCode),
		State: Pending, CreatedAt: now, ExpiresAt: now.Add(authorizationTTL),
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	previous := store.cloneLocked()
	store.pruneLocked(now)
	if !store.data.LastStartedAt.IsZero() && now.Sub(store.data.LastStartedAt) < startInterval {
		store.data = previous
		return StartResult{}, ErrRateLimited
	}
	store.evictAuthorizationLocked()
	if len(store.data.Authorizations) >= maxAuthorizations {
		store.data = previous
		return StartResult{}, ErrCapacity
	}
	store.data.Authorizations[id] = record
	store.data.LastStartedAt = now
	if err := store.commitLocked(previous); err != nil {
		return StartResult{}, err
	}
	return StartResult{id, deviceCode, userCode, record.ExpiresAt, pollInterval}, nil
}

// Poll records the fixed polling interval durably. On PollApproved the caller
// returns the deviceCode argument itself as the bearer; no new secret is minted.
func (store *Store) Poll(deviceCode string, now time.Time) (PollResult, error) {
	if !canonicalBase64(deviceCode, deviceCodeBytes) {
		return PollResult{}, ErrInvalidCode
	}
	now = now.UTC()
	want := digest("device", deviceCode)
	store.mu.Lock()
	defer store.mu.Unlock()
	previous := store.cloneLocked()
	store.pruneFailuresLocked(now)
	record, id, found := findAuthorization(store.data.Authorizations, want)
	if !found {
		if len(store.data.InvalidPolls) >= maxInvalidPolls {
			store.data = previous
			return PollResult{}, ErrRateLimited
		}
		store.data.InvalidPolls = append(store.data.InvalidPolls, now)
		if err := store.commitLocked(previous); err != nil {
			return PollResult{}, err
		}
		return PollResult{}, ErrInvalidCode
	}
	if !record.LastPolledAt.IsZero() && now.Sub(record.LastPolledAt) < pollInterval {
		store.data = previous
		return PollResult{}, ErrRateLimited
	}
	if record.State == Pending && !now.Before(record.ExpiresAt) {
		record.State, record.DecidedAt = Expired, record.ExpiresAt
	}
	record.LastPolledAt = now
	store.data.Authorizations[id] = record
	result := PollResult{State: PollState(record.State)}
	if record.State == Approved {
		credential, ok := store.data.Credentials[record.CredentialID]
		if !ok || credential.State == Revoked {
			result.State = PollDenied
		} else if credential.State == CredentialExpired || !now.Before(credential.ExpiresAt) {
			credential.State = CredentialExpired
			store.data.Credentials[credential.ID] = credential
			result.State = PollExpired
		} else if credential.State == Active {
			result.CredentialID, result.ExpiresAt = credential.ID, credential.ExpiresAt
		} else {
			result.State = PollDenied
		}
	}
	if err := store.commitLocked(previous); err != nil {
		return PollResult{}, err
	}
	return result, nil
}

func (store *Store) Approve(id, userCode string, actor task.ActorSnapshot, now time.Time) (Credential, error) {
	return store.ApproveContext(context.Background(), id, userCode, actor, now)
}

func (store *Store) ApproveContext(ctx context.Context, id, userCode string, actor task.ActorSnapshot, now time.Time) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	attribution, err := trustedAttributionFromActor(actor)
	if err != nil {
		return Credential{}, err
	}
	if !canonicalUserCode(userCode) {
		return Credential{}, ErrInvalidCode
	}
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	previous := store.cloneLocked()
	record, ok := store.data.Authorizations[id]
	if !ok || subtle.ConstantTimeCompare([]byte(record.UserDigest), []byte(digest("user", userCode))) != 1 {
		return Credential{}, ErrNotFound
	}
	if record.State == Approved {
		return store.data.Credentials[record.CredentialID].Credential, nil
	}
	if record.State != Pending {
		return Credential{}, ErrInvalidState
	}
	if !now.Before(record.ExpiresAt) {
		record.State, record.DecidedAt = Expired, record.ExpiresAt
		store.data.Authorizations[id] = record
		if err := store.commitLockedContext(ctx, previous); err != nil {
			return Credential{}, err
		}
		return Credential{}, ErrInvalidState
	}
	store.evictCredentialLocked()
	if len(store.data.Credentials) >= maxCredentials {
		return Credential{}, ErrCapacity
	}
	credentialID, err := randomID(credentialIDTag)
	if err != nil {
		return Credential{}, err
	}
	credential := Credential{
		ID: credentialID, AuthorizationID: id, State: Active, CreatedAt: now,
		ExpiresAt: now.Add(credentialTTL), ApprovedBy: attribution,
	}
	store.data.Credentials[credentialID] = credentialRecord{Credential: credential, DeviceDigest: record.DeviceDigest}
	record.State, record.DecidedAt, record.DecidedBy, record.CredentialID = Approved, now, &attribution, credentialID
	store.data.Authorizations[id] = record
	if err := store.commitLockedContext(ctx, previous); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

func (store *Store) Deny(id, userCode string, actor task.ActorSnapshot, now time.Time) error {
	return store.DenyContext(context.Background(), id, userCode, actor, now)
}

func (store *Store) DenyContext(ctx context.Context, id, userCode string, actor task.ActorSnapshot, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	attribution, err := trustedAttributionFromActor(actor)
	if err != nil {
		return err
	}
	if !canonicalUserCode(userCode) {
		return ErrInvalidCode
	}
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	previous := store.cloneLocked()
	record, ok := store.data.Authorizations[id]
	if !ok || subtle.ConstantTimeCompare([]byte(record.UserDigest), []byte(digest("user", userCode))) != 1 {
		return ErrNotFound
	}
	if record.State == Denied {
		return nil
	}
	if record.State != Pending {
		return ErrInvalidState
	}
	if !now.Before(record.ExpiresAt) {
		record.State = Expired
		record.DecidedAt = record.ExpiresAt
	} else {
		record.State, record.DecidedBy = Denied, &attribution
		record.DecidedAt = now
	}
	store.data.Authorizations[id] = record
	if err := store.commitLockedContext(ctx, previous); err != nil {
		return err
	}
	if record.State == Expired {
		return ErrInvalidState
	}
	return nil
}

// Pending verifies the independent user code without returning either stored
// digest. It performs no transition; expiry is projected by the current time.
func (store *Store) Pending(id, userCode string, now time.Time) bool {
	if !canonicalID(id, authorizationIDTag) || !canonicalUserCode(userCode) {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.data.Authorizations[id]
	return ok && record.State == Pending && now.Before(record.ExpiresAt) &&
		subtle.ConstantTimeCompare([]byte(record.UserDigest), []byte(digest("user", userCode))) == 1
}

func (store *Store) Authenticate(deviceCode string, now time.Time) (Credential, bool, error) {
	if !canonicalBase64(deviceCode, deviceCodeBytes) {
		return Credential{}, false, nil
	}
	want := digest("device", deviceCode)
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	for id, record := range store.data.Credentials {
		if subtle.ConstantTimeCompare([]byte(record.DeviceDigest), []byte(want)) != 1 {
			continue
		}
		if record.State != Active {
			return Credential{}, false, nil
		}
		if !now.Before(record.ExpiresAt) {
			previous := store.cloneLocked()
			record.State = CredentialExpired
			store.data.Credentials[id] = record
			if err := store.commitLocked(previous); err != nil {
				return Credential{}, false, err
			}
			return Credential{}, false, nil
		}
		return record.Credential, true, nil
	}
	return Credential{}, false, nil
}

// RegisterRequest atomically fences request admission against revoke.
func (store *Store) RegisterRequest(id string, now time.Time, cancel context.CancelFunc) (func(), bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.data.Credentials[id]
	if !ok || record.State != Active || !now.Before(record.ExpiresAt) {
		return nil, false
	}
	store.nextRequestID++
	requestID := store.nextRequestID
	requests := store.active[id]
	if requests == nil {
		requests = make(map[uint64]context.CancelFunc)
		store.active[id] = requests
	}
	requests[requestID] = cancel
	var once sync.Once
	return func() {
		once.Do(func() {
			store.mu.Lock()
			defer store.mu.Unlock()
			delete(store.active[id], requestID)
			if len(store.active[id]) == 0 {
				delete(store.active, id)
			}
		})
	}, true
}

func (store *Store) Credentials(now time.Time) ([]Credential, error) {
	now = now.UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	previous := store.cloneLocked()
	changed := store.expireCredentialsLocked(now)
	if changed {
		if err := store.commitLocked(previous); err != nil {
			return nil, err
		}
	}
	result := make([]Credential, 0, len(store.data.Credentials))
	for _, record := range store.data.Credentials {
		result = append(result, record.Credential)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (store *Store) Revoke(id string, actor task.ActorSnapshot, now time.Time) error {
	value, err := AttributionFromActor(actor)
	if err != nil {
		return err
	}
	attribution := &value
	now = now.UTC()
	store.mu.Lock()
	previous := store.cloneLocked()
	record, ok := store.data.Credentials[id]
	if !ok {
		store.mu.Unlock()
		return ErrNotFound
	}
	if record.State == Revoked {
		store.mu.Unlock()
		return nil
	}
	if record.State == Active && !now.Before(record.ExpiresAt) {
		record.State = CredentialExpired
		store.data.Credentials[id] = record
		if err := store.commitLocked(previous); err != nil {
			store.mu.Unlock()
			return err
		}
		store.mu.Unlock()
		return ErrInvalidState
	}
	if record.State != Active {
		store.mu.Unlock()
		return ErrInvalidState
	}
	record.State, record.RevokedAt, record.RevokedBy = Revoked, now, attribution
	store.data.Credentials[id] = record
	if err := store.commitLocked(previous); err != nil {
		store.mu.Unlock()
		return err
	}
	requests := store.active[id]
	delete(store.active, id)
	store.mu.Unlock()
	for _, cancel := range requests {
		cancel()
	}
	return nil
}

func (store *Store) load() error {
	file, err := os.OpenFile(store.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open plugin authorization state: %w", err)
	}
	defer file.Close()
	if err := validatePrivateFile(file); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return fmt.Errorf("read plugin authorization state: %w", err)
	}
	if len(data) > maxStateBytes {
		return errors.New("plugin authorization state exceeds 256 KiB")
	}
	if err := jsoncanon.Check(data, 8); err != nil {
		return fmt.Errorf("decode plugin authorization state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state diskState
	if err := decoder.Decode(&state); err != nil {
		return fmt.Errorf("decode plugin authorization state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode plugin authorization state: trailing data")
	}
	if err := validateState(state, store.workspace); err != nil {
		return err
	}
	store.data = state
	return nil
}

func (store *Store) commitLocked(previous diskState) error {
	return store.commitLockedContext(context.Background(), previous)
}

func (store *Store) commitLockedContext(ctx context.Context, previous diskState) error {
	if err := ctx.Err(); err != nil {
		store.data = previous
		return err
	}
	store.data.Revision++
	if err := validateState(store.data, store.workspace); err != nil {
		store.data = previous
		return fmt.Errorf("validate plugin authorization state: %w", err)
	}
	data, err := json.Marshal(store.data)
	if err != nil || len(data) > maxStateBytes {
		store.data = previous
		if err != nil {
			return fmt.Errorf("encode plugin authorization state: %w", err)
		}
		return errors.New("plugin authorization state exceeds 256 KiB")
	}
	if err := secureExistingPath(store.path); err != nil {
		store.data = previous
		return err
	}
	directory := filepath.Dir(store.path)
	temporary, err := os.CreateTemp(directory, ".pluginauth-*.tmp")
	if err != nil {
		store.data = previous
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	// Cancellation can still safely abort while only the private temporary file
	// exists. Once rename succeeds the transition may be durable, so Fern must
	// finish directory sync and report that commit outcome instead of pretending
	// cancellation rolled it back.
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.Rename(temporaryPath, store.path)
	}
	if err != nil {
		store.data = previous
		return fmt.Errorf("persist plugin authorization state: %w", err)
	}
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("plugin authorization state replaced but directory sync failed: %w", err)
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err != nil {
		return fmt.Errorf("plugin authorization state replaced but directory sync failed: %w", err)
	}
	return nil
}

func validateState(state diskState, workspace string) error {
	if state.Version != stateVersion || state.Workspace != workspace || state.Authorizations == nil || state.Credentials == nil {
		return errors.New("invalid plugin authorization state header")
	}
	if len(state.Authorizations) > maxAuthorizations || len(state.Credentials) > maxCredentials || len(state.InvalidPolls) > maxInvalidPolls {
		return errors.New("plugin authorization state exceeds bounds")
	}
	deviceDigests := make(map[string]struct{}, len(state.Authorizations))
	userDigests := make(map[string]struct{}, len(state.Authorizations))
	for id, record := range state.Authorizations {
		if id != record.ID || !canonicalID(id, authorizationIDTag) || !canonicalDigest(record.DeviceDigest) || !canonicalDigest(record.UserDigest) || record.CreatedAt.IsZero() || !record.ExpiresAt.After(record.CreatedAt) || record.ExpiresAt.Sub(record.CreatedAt) != authorizationTTL {
			return errors.New("invalid plugin authorization record")
		}
		if _, exists := deviceDigests[record.DeviceDigest]; exists {
			return errors.New("duplicate plugin device-code digest")
		}
		if _, exists := userDigests[record.UserDigest]; exists {
			return errors.New("duplicate plugin user-code digest")
		}
		deviceDigests[record.DeviceDigest], userDigests[record.UserDigest] = struct{}{}, struct{}{}
		if !record.LastPolledAt.IsZero() && record.LastPolledAt.Before(record.CreatedAt) {
			return errors.New("invalid plugin authorization poll time")
		}
		switch record.State {
		case Pending:
			if !record.DecidedAt.IsZero() || record.DecidedBy != nil || record.CredentialID != "" {
				return errors.New("invalid pending plugin authorization")
			}
		case Approved:
			credential, ok := state.Credentials[record.CredentialID]
			if !ok || record.DecidedAt.Before(record.CreatedAt) || !record.DecidedAt.Before(record.ExpiresAt) || record.DecidedBy == nil || credential.AuthorizationID != id || credential.DeviceDigest != record.DeviceDigest || credential.ApprovedBy != *record.DecidedBy {
				return errors.New("invalid approved plugin authorization")
			}
		case Denied:
			if record.DecidedAt.Before(record.CreatedAt) || !record.DecidedAt.Before(record.ExpiresAt) || record.DecidedBy == nil || record.CredentialID != "" {
				return errors.New("invalid denied plugin authorization")
			}
		case Expired:
			if record.DecidedAt.Before(record.ExpiresAt) || record.DecidedBy != nil || record.CredentialID != "" {
				return errors.New("invalid expired plugin authorization")
			}
		default:
			return errors.New("invalid plugin authorization state")
		}
		if record.DecidedBy != nil && !validAttribution(*record.DecidedBy) {
			return errors.New("invalid plugin authorization attribution")
		}
		if record.DecidedBy != nil && record.DecidedBy.Type != task.ActorDevice && record.DecidedBy.Type != task.ActorOperator {
			return errors.New("untrusted plugin authorization decision attribution")
		}
	}
	for id, record := range state.Credentials {
		if id != record.ID || !canonicalID(id, credentialIDTag) || !canonicalID(record.AuthorizationID, authorizationIDTag) || !canonicalDigest(record.DeviceDigest) || record.CreatedAt.IsZero() || record.ExpiresAt.Sub(record.CreatedAt) != credentialTTL || !validAttribution(record.ApprovedBy) {
			return errors.New("invalid plugin credential record")
		}
		authorization, ok := state.Authorizations[record.AuthorizationID]
		if !ok || authorization.State != Approved || authorization.CredentialID != id || !record.CreatedAt.Equal(authorization.DecidedAt) {
			return errors.New("orphaned plugin credential record")
		}
		switch record.State {
		case Active, CredentialExpired:
			if !record.RevokedAt.IsZero() || record.RevokedBy != nil {
				return errors.New("invalid plugin credential lifecycle")
			}
		case Revoked:
			if record.RevokedAt.Before(record.CreatedAt) {
				return errors.New("invalid revoked plugin credential")
			}
		default:
			return errors.New("invalid plugin credential state")
		}
		if record.RevokedBy != nil && !validAttribution(*record.RevokedBy) {
			return errors.New("invalid plugin credential revocation attribution")
		}
	}
	for index, value := range state.InvalidPolls {
		if value.IsZero() {
			return errors.New("invalid plugin authorization limiter time")
		}
		if index != 0 && value.Before(state.InvalidPolls[index-1]) {
			return errors.New("unsorted plugin authorization limiter times")
		}
	}
	return nil
}

func validAttribution(value Attribution) bool {
	actor := task.ActorSnapshot{Type: value.Type, ID: value.ID, DisplayName: value.DisplayName, CredentialID: value.CredentialID, Authentication: value.Authentication, RequestID: value.RequestID}
	return actor.Validate() == nil
}

func validatePrivateFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("plugin authorization state must be a private singly linked regular file")
	}
	return nil
}

func secureExistingPath(path string) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect plugin authorization state: %w", err)
	}
	defer file.Close()
	return validatePrivateFile(file)
}

func emptyState(workspace string) diskState {
	return diskState{Version: stateVersion, Workspace: workspace, Authorizations: make(map[string]authorizationRecord), Credentials: make(map[string]credentialRecord)}
}

func (store *Store) cloneLocked() diskState {
	clone := store.data
	clone.InvalidPolls = append([]time.Time(nil), store.data.InvalidPolls...)
	clone.Authorizations = make(map[string]authorizationRecord, len(store.data.Authorizations))
	for id, record := range store.data.Authorizations {
		if record.DecidedBy != nil {
			value := *record.DecidedBy
			record.DecidedBy = &value
		}
		clone.Authorizations[id] = record
	}
	clone.Credentials = make(map[string]credentialRecord, len(store.data.Credentials))
	for id, record := range store.data.Credentials {
		if record.RevokedBy != nil {
			value := *record.RevokedBy
			record.RevokedBy = &value
		}
		clone.Credentials[id] = record
	}
	return clone
}

func (store *Store) pruneLocked(now time.Time) {
	store.pruneFailuresLocked(now)
	store.expireCredentialsLocked(now)
	for id, record := range store.data.Authorizations {
		if record.State == Pending && !now.Before(record.ExpiresAt) {
			record.State, record.DecidedAt = Expired, record.ExpiresAt
			store.data.Authorizations[id] = record
		}
		_, hasCredential := store.data.Credentials[record.CredentialID]
		if record.State != Pending && (!hasCredential || record.CredentialID == "") && !record.DecidedAt.IsZero() && !now.Before(record.DecidedAt.Add(terminalRetention)) {
			delete(store.data.Authorizations, id)
		}
	}
	for id, record := range store.data.Credentials {
		terminalAt := record.RevokedAt
		if record.State == CredentialExpired {
			terminalAt = record.ExpiresAt
		}
		if record.State != Active && !terminalAt.IsZero() && !now.Before(terminalAt.Add(terminalRetention)) {
			delete(store.data.Credentials, id)
			delete(store.data.Authorizations, record.AuthorizationID)
		}
	}
}

// The retention window is best effort under the fixed file caps. Terminal
// records are oldest-first eviction candidates; pending and active authority is
// never displaced to admit a new request.
func (store *Store) evictAuthorizationLocked() {
	for len(store.data.Authorizations) >= maxAuthorizations {
		oldestID := ""
		var oldest time.Time
		for id, record := range store.data.Authorizations {
			if record.State == Pending {
				continue
			}
			credential, hasCredential := store.data.Credentials[record.CredentialID]
			if hasCredential && credential.State == Active {
				continue
			}
			if oldestID == "" || record.DecidedAt.Before(oldest) {
				oldestID, oldest = id, record.DecidedAt
			}
		}
		if oldestID == "" {
			return
		}
		credentialID := store.data.Authorizations[oldestID].CredentialID
		delete(store.data.Authorizations, oldestID)
		delete(store.data.Credentials, credentialID)
	}
}

func (store *Store) evictCredentialLocked() {
	for len(store.data.Credentials) >= maxCredentials {
		oldestID := ""
		var oldest time.Time
		for id, record := range store.data.Credentials {
			if record.State == Active {
				continue
			}
			terminalAt := record.RevokedAt
			if record.State == CredentialExpired {
				terminalAt = record.ExpiresAt
			}
			if oldestID == "" || terminalAt.Before(oldest) {
				oldestID, oldest = id, terminalAt
			}
		}
		if oldestID == "" {
			return
		}
		authorizationID := store.data.Credentials[oldestID].AuthorizationID
		delete(store.data.Credentials, oldestID)
		delete(store.data.Authorizations, authorizationID)
	}
}

func (store *Store) pruneFailuresLocked(now time.Time) {
	first := 0
	for first < len(store.data.InvalidPolls) && !now.Before(store.data.InvalidPolls[first].Add(failureWindow)) {
		first++
	}
	store.data.InvalidPolls = append([]time.Time(nil), store.data.InvalidPolls[first:]...)
}

func (store *Store) expireCredentialsLocked(now time.Time) bool {
	changed := false
	for id, record := range store.data.Credentials {
		if record.State == Active && !now.Before(record.ExpiresAt) {
			record.State = CredentialExpired
			store.data.Credentials[id] = record
			changed = true
		}
	}
	return changed
}

func findAuthorization(records map[string]authorizationRecord, digest string) (authorizationRecord, string, bool) {
	for id, record := range records {
		if subtle.ConstantTimeCompare([]byte(record.DeviceDigest), []byte(digest)) == 1 {
			return record, id, true
		}
	}
	return authorizationRecord{}, "", false
}

func digest(domain, value string) string {
	sum := sha256.Sum256([]byte("fern-plugin-auth-v1\x00" + domain + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

func randomBase64(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomID(prefix string) (string, error) {
	value, err := randomBase64(randomIDBytes)
	return prefix + value, err
}

func randomUserCode() (string, error) {
	value := make([]byte, userCodeBytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(value)
	return encoded[:5] + "-" + encoded[5:10] + "-" + encoded[10:], nil
}

func canonicalBase64(value string, size int) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func canonicalUserCode(value string) bool {
	if len(value) != 15 || value[5] != '-' || value[11] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(compact)
	return err == nil && len(decoded) == userCodeBytes && base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded) == compact
}

func canonicalID(value, prefix string) bool {
	suffix, ok := strings.CutPrefix(value, prefix)
	return ok && canonicalBase64(suffix, randomIDBytes)
}

func canonicalDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}
