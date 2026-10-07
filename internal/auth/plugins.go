package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/domain"
)

const (
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
	Type           domain.ActorType `json:"type"`
	ID             string           `json:"id"`
	DisplayName    string           `json:"displayName,omitempty"`
	CredentialID   string           `json:"credentialId"`
	Authentication string           `json:"authentication"`
	RequestID      string           `json:"requestId"`
}

func AttributionFromActor(actor domain.ActorSnapshot) (Attribution, error) {
	if err := actor.Validate(); err != nil {
		return Attribution{}, errors.New("valid plugin authorization actor is required")
	}
	return Attribution{actor.Type, actor.ID, actor.DisplayName, actor.CredentialID, actor.Authentication, actor.RequestID}, nil
}

func trustedAttributionFromActor(actor domain.ActorSnapshot) (Attribution, error) {
	value, err := AttributionFromActor(actor)
	if err != nil || actor.Type != domain.ActorDevice && actor.Type != domain.ActorOperator {
		return Attribution{}, errors.New("trusted approval actor is required")
	}
	return value, nil
}

// PluginStore is the plugin authorization state in Fern's SQLite database. Each
// operation is one transaction; mu only fences the in-memory request registry
// against revocation.
type PluginStore struct {
	db            *sql.DB
	mu            sync.Mutex
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

// NewPluginStore returns the plugin authorization store backed by db, whose schema
// (store) defines the plugin_* tables.
func NewPluginStore(db *sql.DB) *PluginStore {
	return &PluginStore{db: db, active: make(map[string]map[uint64]context.CancelFunc)}
}

func (store *PluginStore) Start(now time.Time) (StartResult, error) {
	now = now.UTC()
	deviceCode, userCode, id := domain.NewSecret(), randomUserCode(), randomID(authorizationIDTag)
	expiresAt := now.Add(authorizationTTL)
	err := store.transact(context.Background(), func(tx *sql.Tx) error {
		if err := prune(tx, now); err != nil {
			return err
		}
		var lastStarted sql.NullInt64
		if err := tx.QueryRow(`SELECT max(created_at) FROM plugin_authorizations`).Scan(&lastStarted); err != nil {
			return err
		}
		if lastStarted.Valid && now.Sub(fromNanos(lastStarted)) < startInterval {
			return ErrRateLimited
		}
		// Terminal records are oldest-first eviction candidates; pending and
		// active authority is never displaced to admit a new request.
		if err := evict(tx, `plugin_authorizations`, maxAuthorizations, `SELECT a.id FROM plugin_authorizations a
LEFT JOIN plugin_credentials c ON c.authorization_id=a.id
WHERE a.state<>'pending' AND (c.state IS NULL OR c.state<>'active') ORDER BY a.decided_at LIMIT 1`); err != nil {
			return err
		}
		if full, err := atCapacity(tx, `plugin_authorizations`, maxAuthorizations); err != nil {
			return err
		} else if full {
			return ErrCapacity
		}
		_, err := tx.Exec(`INSERT INTO plugin_authorizations(id,device_sha256,user_sha256,state,created_at,expires_at)
VALUES(?,?,?,?,?,?)`, id, digest("device", deviceCode), digest("user", userCode), Pending, now.UnixNano(), expiresAt.UnixNano())
		return err
	})
	if err != nil {
		return StartResult{}, err
	}
	return StartResult{id, deviceCode, userCode, expiresAt, pollInterval}, nil
}

// Poll records the fixed polling interval durably. On PollApproved the caller
// returns the deviceCode argument itself as the bearer; no new secret is minted.
func (store *PluginStore) Poll(deviceCode string, now time.Time) (PollResult, error) {
	if !canonicalBase64(deviceCode, deviceCodeBytes) {
		return PollResult{}, ErrInvalidCode
	}
	now = now.UTC()
	var result PollResult
	var outcome error
	err := store.transact(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM plugin_invalid_polls WHERE at<=?`, now.Add(-failureWindow).UnixNano()); err != nil {
			return err
		}
		var id string
		var state AuthorizationState
		var expiresAt int64
		var lastPolled sql.NullInt64
		err := tx.QueryRow(`SELECT id,state,expires_at,last_polled_at FROM plugin_authorizations WHERE device_sha256=?`,
			digest("device", deviceCode)).Scan(&id, &state, &expiresAt, &lastPolled)
		if errors.Is(err, sql.ErrNoRows) {
			if full, err := atCapacity(tx, `plugin_invalid_polls`, maxInvalidPolls); err != nil {
				return err
			} else if full {
				return ErrRateLimited
			}
			// The failed attempt itself is durable rate-limit evidence.
			outcome = ErrInvalidCode
			_, err := tx.Exec(`INSERT INTO plugin_invalid_polls(at) VALUES(?)`, now.UnixNano())
			return err
		}
		if err != nil {
			return err
		}
		if lastPolled.Valid && now.Sub(fromNanos(lastPolled)) < pollInterval {
			return ErrRateLimited
		}
		if state == Pending && now.UnixNano() >= expiresAt {
			state = Expired
			if _, err := tx.Exec(`UPDATE plugin_authorizations SET state='expired',decided_at=expires_at WHERE id=?`, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE plugin_authorizations SET last_polled_at=? WHERE id=?`, now.UnixNano(), id); err != nil {
			return err
		}
		result = PollResult{State: PollState(state)}
		if state != Approved {
			return nil
		}
		credential, err := scanCredential(tx.QueryRow(`SELECT `+credentialColumns+` FROM plugin_credentials WHERE authorization_id=?`, id))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			result.State = PollDenied
		case err != nil:
			return err
		case credential.State == CredentialExpired || credential.State == Active && !now.Before(credential.ExpiresAt):
			result.State = PollExpired
			_, err = tx.Exec(`UPDATE plugin_credentials SET state='expired' WHERE id=?`, credential.ID)
			return err
		case credential.State == Active:
			result.CredentialID, result.ExpiresAt = credential.ID, credential.ExpiresAt
		default:
			result.State = PollDenied
		}
		return nil
	})
	if err != nil {
		return PollResult{}, err
	}
	if outcome != nil {
		return PollResult{}, outcome
	}
	return result, nil
}

func (store *PluginStore) Approve(ctx context.Context, id, userCode string, actor domain.ActorSnapshot, now time.Time) (Credential, error) {
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
	var credential Credential
	err = store.transact(ctx, func(tx *sql.Tx) error {
		state, expiresAt, err := pendingAuthorization(tx, id, userCode)
		if err != nil {
			return err
		}
		if state == Approved {
			credential, err = scanCredential(tx.QueryRow(`SELECT `+credentialColumns+` FROM plugin_credentials WHERE authorization_id=?`, id))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInvalidState
			}
			return err
		}
		if state != Pending || !now.Before(expiresAt) {
			return ErrInvalidState
		}
		if err := evict(tx, `plugin_credentials`, maxCredentials, `SELECT authorization_id FROM plugin_credentials
WHERE state<>'active' ORDER BY CASE state WHEN 'expired' THEN expires_at ELSE revoked_at END LIMIT 1`); err != nil {
			return err
		}
		if full, err := atCapacity(tx, `plugin_credentials`, maxCredentials); err != nil {
			return err
		} else if full {
			return ErrCapacity
		}
		credentialID := randomID(credentialIDTag)
		credential = Credential{ID: credentialID, AuthorizationID: id, State: Active, CreatedAt: now,
			ExpiresAt: now.Add(credentialTTL), ApprovedBy: attribution}
		approvedBy, err := json.Marshal(attribution)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE plugin_authorizations SET state='approved',decided_at=?,decided_by=? WHERE id=?`,
			now.UnixNano(), string(approvedBy), id); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO plugin_credentials(id,authorization_id,state,created_at,expires_at,approved_by)
VALUES(?,?,'active',?,?,?)`, credentialID, id, now.UnixNano(), credential.ExpiresAt.UnixNano(), string(approvedBy))
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return credential, nil
}

func (store *PluginStore) Deny(ctx context.Context, id, userCode string, actor domain.ActorSnapshot, now time.Time) error {
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
	return store.transact(ctx, func(tx *sql.Tx) error {
		state, expiresAt, err := pendingAuthorization(tx, id, userCode)
		if err != nil {
			return err
		}
		if state == Denied {
			return nil
		}
		if state != Pending || !now.Before(expiresAt) {
			return ErrInvalidState
		}
		deniedBy, err := json.Marshal(attribution)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE plugin_authorizations SET state='denied',decided_at=?,decided_by=? WHERE id=?`,
			now.UnixNano(), string(deniedBy), id)
		return err
	})
}

// Pending verifies the independent user code without returning either stored
// digest. It performs no transition; expiry is projected by the current time.
func (store *PluginStore) Pending(id, userCode string, now time.Time) bool {
	if !canonicalID(id, authorizationIDTag) || !canonicalUserCode(userCode) {
		return false
	}
	state, expiresAt, err := pendingAuthorization(store.db, id, userCode)
	return err == nil && state == Pending && now.Before(expiresAt)
}

// Authenticate resolves a presented bearer to its active credential. It never
// writes: an expired credential is merely rejected here and marked expired by
// the next listing, poll, or start.
func (store *PluginStore) Authenticate(deviceCode string, now time.Time) (Credential, bool, error) {
	if !canonicalBase64(deviceCode, deviceCodeBytes) {
		return Credential{}, false, nil
	}
	credential, err := scanCredential(store.db.QueryRow(`SELECT `+credentialColumns+` FROM plugin_credentials
JOIN plugin_authorizations a ON a.id=authorization_id WHERE a.device_sha256=?`, digest("device", deviceCode)))
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, false, nil
	}
	if err != nil {
		return Credential{}, false, fmt.Errorf("read plugin credential: %w", err)
	}
	if credential.State != Active || !now.Before(credential.ExpiresAt) {
		return Credential{}, false, nil
	}
	return credential, true, nil
}

// RegisterRequest atomically fences request admission against revoke.
func (store *PluginStore) RegisterRequest(id string, now time.Time, cancel context.CancelFunc) (func(), bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var active bool
	if err := store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM plugin_credentials WHERE id=? AND state='active' AND expires_at>?)`,
		id, now.UnixNano()).Scan(&active); err != nil || !active {
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

// Credentials lists every retained credential newest-first, durably marking
// lapsed active credentials expired.
func (store *PluginStore) Credentials(now time.Time) ([]Credential, error) {
	result := []Credential{}
	err := store.transact(context.Background(), func(tx *sql.Tx) error {
		if err := expireCredentials(tx, now); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT ` + credentialColumns + ` FROM plugin_credentials ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			credential, err := scanCredential(rows)
			if err != nil {
				return err
			}
			result = append(result, credential)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Revoke durably revokes an active credential, then cancels every request
// registered against it.
func (store *PluginStore) Revoke(id string, actor domain.ActorSnapshot, now time.Time) error {
	attribution, err := AttributionFromActor(actor)
	if err != nil {
		return err
	}
	revokedBy, err := json.Marshal(attribution)
	if err != nil {
		return err
	}
	store.mu.Lock()
	err = store.transact(context.Background(), func(tx *sql.Tx) error {
		var state CredentialState
		var expiresAt int64
		err := tx.QueryRow(`SELECT state,expires_at FROM plugin_credentials WHERE id=?`, id).Scan(&state, &expiresAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case state == Revoked:
			return nil
		case state != Active || now.UnixNano() >= expiresAt:
			return ErrInvalidState
		}
		_, err = tx.Exec(`UPDATE plugin_credentials SET state='revoked',revoked_at=?,revoked_by=? WHERE id=?`,
			now.UnixNano(), string(revokedBy), id)
		return err
	})
	requests := store.active[id]
	if err == nil {
		delete(store.active, id)
	}
	store.mu.Unlock()
	if err != nil {
		return err
	}
	for _, cancel := range requests {
		cancel()
	}
	return nil
}

// transact runs fn in one immediate SQLite transaction, committing only when
// fn succeeds and ctx is still live.
func (store *PluginStore) transact(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin plugin authorization transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("persist plugin authorization state: %w", err)
	}
	return nil
}

type queryRower interface {
	QueryRow(string, ...any) *sql.Row
}

// pendingAuthorization loads an authorization by ID after a constant-time
// comparison of the presented user code's digest.
func pendingAuthorization(q queryRower, id, userCode string) (AuthorizationState, time.Time, error) {
	var state AuthorizationState
	var userDigest string
	var expiresAt int64
	err := q.QueryRow(`SELECT state,user_sha256,expires_at FROM plugin_authorizations WHERE id=?`, id).Scan(&state, &userDigest, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && subtle.ConstantTimeCompare([]byte(userDigest), []byte(digest("user", userCode))) != 1 {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return state, time.Unix(0, expiresAt).UTC(), nil
}

const credentialColumns = `plugin_credentials.id,authorization_id,plugin_credentials.state,plugin_credentials.created_at,
plugin_credentials.expires_at,revoked_at,approved_by,revoked_by`

func scanCredential(row interface{ Scan(...any) error }) (Credential, error) {
	var credential Credential
	var createdAt, expiresAt int64
	var revokedAt sql.NullInt64
	var approvedBy string
	var revokedBy sql.NullString
	if err := row.Scan(&credential.ID, &credential.AuthorizationID, &credential.State, &createdAt, &expiresAt,
		&revokedAt, &approvedBy, &revokedBy); err != nil {
		return Credential{}, err
	}
	credential.CreatedAt, credential.ExpiresAt = time.Unix(0, createdAt).UTC(), time.Unix(0, expiresAt).UTC()
	if revokedAt.Valid {
		credential.RevokedAt = fromNanos(revokedAt)
	}
	if err := json.Unmarshal([]byte(approvedBy), &credential.ApprovedBy); err != nil {
		return Credential{}, err
	}
	if revokedBy.Valid {
		credential.RevokedBy = new(Attribution)
		if err := json.Unmarshal([]byte(revokedBy.String), credential.RevokedBy); err != nil {
			return Credential{}, err
		}
	}
	return credential, nil
}

// prune applies time-driven transitions: expired pending authorizations and
// credentials become terminal, stale invalid polls leave the rate window, and
// terminal records older than the retention window are deleted.
func prune(tx *sql.Tx, now time.Time) error {
	if err := expireCredentials(tx, now); err != nil {
		return err
	}
	retained := now.Add(-terminalRetention).UnixNano()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM plugin_invalid_polls WHERE at<=?`, []any{now.Add(-failureWindow).UnixNano()}},
		{`UPDATE plugin_authorizations SET state='expired',decided_at=expires_at WHERE state='pending' AND expires_at<=?`, []any{now.UnixNano()}},
		{`DELETE FROM plugin_authorizations WHERE state<>'pending' AND decided_at<=?
AND id NOT IN (SELECT authorization_id FROM plugin_credentials)`, []any{retained}},
		{`DELETE FROM plugin_authorizations WHERE id IN (SELECT authorization_id FROM plugin_credentials
WHERE state<>'active' AND CASE state WHEN 'expired' THEN expires_at ELSE revoked_at END<=?)`, []any{retained}},
	} {
		if _, err := tx.Exec(statement.sql, statement.args...); err != nil {
			return err
		}
	}
	return nil
}

func expireCredentials(tx *sql.Tx, now time.Time) error {
	_, err := tx.Exec(`UPDATE plugin_credentials SET state='expired' WHERE state='active' AND expires_at<=?`, now.UnixNano())
	return err
}

// evict deletes the authorization (and so its credential) named by
// oldestQuery until table has room for one more row or nothing is evictable.
func evict(tx *sql.Tx, table string, limit int, oldestQuery string) error {
	for {
		full, err := atCapacity(tx, table, limit)
		if err != nil || !full {
			return err
		}
		result, err := tx.Exec(`DELETE FROM plugin_authorizations WHERE id=(` + oldestQuery + `)`)
		if err != nil {
			return err
		}
		if deleted, err := result.RowsAffected(); err != nil || deleted == 0 {
			return err
		}
	}
}

func atCapacity(tx *sql.Tx, table string, limit int) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count)
	return count >= limit, err
}

func fromNanos(value sql.NullInt64) time.Time { return time.Unix(0, value.Int64).UTC() }

func digest(label, value string) string {
	sum := sha256.Sum256([]byte("fern-plugin-auth-v1\x00" + label + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

func randomID(prefix string) string {
	return prefix + base64.RawURLEncoding.EncodeToString(randomBytes(randomIDBytes))
}

func randomUserCode() string {
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes(userCodeBytes))
	return encoded[:5] + "-" + encoded[5:10] + "-" + encoded[10:]
}

// randomBytes reads size bytes from crypto/rand, which never returns an error
// (it aborts the process instead) since Go 1.24.
func randomBytes(size int) []byte {
	value := make([]byte, size)
	_, _ = rand.Read(value)
	return value
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
