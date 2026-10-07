package githubapp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxOnboardingStates              = 64
	maxOnboardingActiveStates        = 16
	maxOnboardingStateLifetime       = 10 * time.Minute
	maxOnboardingReplayWindow        = time.Hour
	maxOnboardingFlowIDBytes         = 128
	maxOnboardingReturnPath          = 1024
	maxOnboardingClaimIDBytes        = 128
	onboardingStateStatusPending     = "pending"
	onboardingStateStatusClaimed     = "claimed"
	onboardingStateStatusCompleted   = "completed"
	onboardingStateStatusQuarantined = "quarantined"
	quarantineReasonClaimExpired     = "claim_expired"
)

var (
	ErrOnboardingStateStoreIO          = errors.New("GitHub App onboarding state store operation failed")
	ErrInvalidOnboardingState          = errors.New("invalid GitHub App onboarding state request")
	ErrOnboardingStateConflict         = errors.New("GitHub App onboarding state request conflicts with an outstanding request")
	ErrOnboardingStateLimit            = errors.New("too many outstanding GitHub App onboarding requests")
	ErrOnboardingStateRejected         = errors.New("GitHub App onboarding state was rejected")
	ErrOnboardingStateRecoveryRequired = errors.New("GitHub App onboarding state requires reconciliation")
)

// OnboardingFlowBinding ties an onboarding state to one local flow and one
// same-origin return path. ReturnPath must begin with one slash, not two.
type OnboardingFlowBinding struct {
	FlowID     string
	ReturnPath string
}

// CallbackClaimDisposition tells the callback coordinator what it may do next.
// Only CallbackClaimExchangeOnce authorizes a manifest exchange.
type CallbackClaimDisposition string

const (
	CallbackClaimExchangeOnce  CallbackClaimDisposition = "exchange_once"
	CallbackClaimReconcileOnly CallbackClaimDisposition = "reconcile_only"
)

// CallbackQuarantineReason is a bounded, non-sensitive reason for closing an
// ambiguous callback claim.
type CallbackQuarantineReason string

const (
	CallbackQuarantineExchangeAmbiguous  CallbackQuarantineReason = "exchange_ambiguous"
	CallbackQuarantineReconcileAmbiguous CallbackQuarantineReason = "reconcile_ambiguous"
	CallbackQuarantineCoordinatorAborted CallbackQuarantineReason = "coordinator_aborted"
)

// CallbackClaim is an immutable value projection and completion fence.
type CallbackClaim struct {
	binding     OnboardingFlowBinding
	issuedAt    time.Time
	expiresAt   time.Time
	claimedAt   time.Time
	disposition CallbackClaimDisposition
	replayed    bool
	stateHash   [sha256.Size]byte
	codeHash    [sha256.Size]byte
	claimHash   [sha256.Size]byte
	flowID      string
	returnPath  string
	valid       bool
}

func (claim CallbackClaim) Binding() OnboardingFlowBinding        { return claim.binding }
func (claim CallbackClaim) IssuedAt() time.Time                   { return claim.issuedAt }
func (claim CallbackClaim) ExpiresAt() time.Time                  { return claim.expiresAt }
func (claim CallbackClaim) ClaimedAt() time.Time                  { return claim.claimedAt }
func (claim CallbackClaim) Disposition() CallbackClaimDisposition { return claim.disposition }
func (claim CallbackClaim) Replayed() bool                        { return claim.replayed }

// OnboardingStateStore persists bounded, one-use callback states in the
// onboarding_states table of Fern's SQLite database (schema owned by
// taskstore). Raw state, callback code, and claim ID values are never
// persisted; each operation is one transaction.
type OnboardingStateStore struct {
	db *sql.DB
}

// NewOnboardingStateStore returns the onboarding state store backed by db.
func NewOnboardingStateStore(db *sql.DB) *OnboardingStateStore {
	return &OnboardingStateStore{db: db}
}

// Begin records a caller-generated, unpadded base64url state containing exactly
// 32 random bytes. now and expiresAt must be UTC, and the lifetime is capped at
// ten minutes.
func (store *OnboardingStateStore) Begin(ctx context.Context, state string, binding OnboardingFlowBinding, now, expiresAt time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	stateHash, err := onboardingStateHash(state)
	if err != nil || !validOnboardingBinding(binding) || !validOnboardingInterval(now, expiresAt) {
		return ErrInvalidOnboardingState
	}
	return store.transact(ctx, now, func(tx *sql.Tx) error {
		var total, active, duplicate, flowActive int
		if err := tx.QueryRow(`SELECT count(*), coalesce(sum(status IN ('pending','claimed')),0),
coalesce(sum(state_sha256=?),0), coalesce(sum(status IN ('pending','claimed') AND flow_id=?),0)
FROM onboarding_states`, stateHash[:], binding.FlowID).Scan(&total, &active, &duplicate, &flowActive); err != nil {
			return err
		}
		if duplicate != 0 || flowActive != 0 {
			return ErrOnboardingStateConflict
		}
		if total >= maxOnboardingStates || active >= maxOnboardingActiveStates {
			return ErrOnboardingStateLimit
		}
		_, err := tx.Exec(`INSERT INTO onboarding_states(state_sha256,status,flow_id,return_path,issued_at,expires_at)
VALUES(?,?,?,?,?,?)`, stateHash[:], onboardingStateStatusPending, binding.FlowID, binding.ReturnPath, now.UnixNano(), expiresAt.UnixNano())
		return err
	})
}

// ResolvePending returns the persisted binding for an unclaimed live state.
// It grants no exchange authority; Claim remains the only effect fence.
func (store *OnboardingStateStore) ResolvePending(ctx context.Context, state string, now time.Time) (OnboardingFlowBinding, time.Time, error) {
	if err := contextError(ctx); err != nil {
		return OnboardingFlowBinding{}, time.Time{}, err
	}
	stateHash, err := onboardingStateHash(state)
	if err != nil || !validUTC(now) {
		return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateRejected
	}
	var entry onboardingStateEntry
	err = store.transact(ctx, now, func(tx *sql.Tx) error {
		var loadErr error
		if entry, loadErr = loadOnboardingState(tx, stateHash); loadErr != nil {
			return loadErr
		}
		if entry.status != onboardingStateStatusPending || now.Before(entry.issuedAt) || !entry.expiresAt.After(now) {
			return ErrOnboardingStateRecoveryRequired
		}
		return nil
	})
	if err != nil {
		return OnboardingFlowBinding{}, time.Time{}, err
	}
	return OnboardingFlowBinding{FlowID: entry.flowID, ReturnPath: entry.returnPath}, entry.expiresAt, nil
}

// Claim durably fences a pending callback before any manifest exchange. An
// exact retry receives the same fence with reconcile-only disposition. A
// changed state binding, code digest, or claim ID is rejected identically.
func (store *OnboardingStateStore) Claim(ctx context.Context, state string, binding OnboardingFlowBinding, callbackCodeDigest [sha256.Size]byte, claimID string, now time.Time) (CallbackClaim, error) {
	if err := contextError(ctx); err != nil {
		return CallbackClaim{}, err
	}
	stateHash, err := onboardingStateHash(state)
	if err != nil || !validOnboardingBinding(binding) || !validOnboardingClaimID(claimID) || callbackCodeDigest == ([sha256.Size]byte{}) || !validUTC(now) {
		return CallbackClaim{}, ErrOnboardingStateRejected
	}
	claimHash := sha256.Sum256([]byte(claimID))
	var claim CallbackClaim
	err = store.transact(ctx, now, func(tx *sql.Tx) error {
		entry, err := loadOnboardingState(tx, stateHash)
		if err != nil {
			return err
		}
		fenceMatches := entry.flowID == binding.FlowID && entry.returnPath == binding.ReturnPath
		if now.Before(entry.issuedAt) {
			return ErrOnboardingStateRejected
		}
		exactReplay := fenceMatches && digestEqual(entry.codeHash, callbackCodeDigest) && digestEqual(entry.claimHash, claimHash)
		switch entry.status {
		case onboardingStateStatusPending:
			if !fenceMatches || !entry.expiresAt.After(now) {
				return ErrOnboardingStateRejected
			}
			// A code or claim ID already bound to another state cannot claim this one.
			var inUse bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM onboarding_states WHERE status<>'pending' AND state_sha256<>?
AND (code_sha256=? OR claim_sha256=?))`, stateHash[:], callbackCodeDigest[:], claimHash[:]).Scan(&inUse); err != nil {
				return err
			}
			if inUse {
				return ErrOnboardingStateRejected
			}
			entry.status, entry.codeHash, entry.claimHash, entry.claimedAt = onboardingStateStatusClaimed, callbackCodeDigest, claimHash, now
			if _, err := tx.Exec(`UPDATE onboarding_states SET status=?,code_sha256=?,claim_sha256=?,claimed_at=? WHERE state_sha256=?`,
				entry.status, callbackCodeDigest[:], claimHash[:], now.UnixNano(), stateHash[:]); err != nil {
				return err
			}
			claim = entry.claim(CallbackClaimExchangeOnce, false)
			return nil
		case onboardingStateStatusClaimed:
			if !exactReplay {
				return ErrOnboardingStateRejected
			}
			claim = entry.claim(CallbackClaimReconcileOnly, true)
			return nil
		default: // completed or quarantined
			if exactReplay {
				return ErrOnboardingStateRecoveryRequired
			}
			return ErrOnboardingStateRejected
		}
	})
	if err != nil {
		return CallbackClaim{}, err
	}
	return claim, nil
}

// Complete atomically closes an exact claim fence. Exact completion replay is
// stable while its bounded tombstone is retained.
func (store *OnboardingStateStore) Complete(ctx context.Context, claim CallbackClaim, now time.Time) error {
	return store.closeClaim(ctx, claim, "", now)
}

// Quarantine atomically closes an exact ambiguous claim. It never restores
// exchange authority, and its retained tombstone requires reconciliation on an
// exact callback replay.
func (store *OnboardingStateStore) Quarantine(ctx context.Context, claim CallbackClaim, reason CallbackQuarantineReason, now time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !validCallbackQuarantineReason(reason) {
		return ErrOnboardingStateRejected
	}
	return store.closeClaim(ctx, claim, string(reason), now)
}

func (store *OnboardingStateStore) closeClaim(ctx context.Context, claim CallbackClaim, reason string, now time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !validUTC(now) || !claim.validFence() {
		return ErrOnboardingStateRejected
	}
	return store.transact(ctx, now, func(tx *sql.Tx) error {
		entry, err := loadOnboardingState(tx, claim.stateHash)
		if err != nil {
			return err
		}
		if !entry.matchesClaim(claim) {
			return ErrOnboardingStateRejected
		}
		replayed := reason == "" && entry.status == onboardingStateStatusCompleted ||
			reason != "" && entry.status == onboardingStateStatusQuarantined && entry.quarantineReason == reason
		switch {
		case replayed && now.Before(entry.closedAt):
			return ErrOnboardingStateRejected
		case replayed:
			return nil
		case entry.status != onboardingStateStatusClaimed:
			return ErrOnboardingStateRecoveryRequired
		case now.Before(entry.claimedAt):
			return ErrOnboardingStateRejected
		case !entry.expiresAt.After(now):
			return ErrOnboardingStateRecoveryRequired
		}
		status := onboardingStateStatusCompleted
		if reason != "" {
			status = onboardingStateStatusQuarantined
		}
		_, err = tx.Exec(`UPDATE onboarding_states SET status=?,closed_at=?,retain_until=?,quarantine_reason=? WHERE state_sha256=?`,
			status, now.UnixNano(), entry.expiresAt.Add(maxOnboardingReplayWindow).UnixNano(),
			sql.NullString{String: reason, Valid: reason != ""}, claim.stateHash[:])
		return err
	})
}

type onboardingStateEntry struct {
	status           string
	stateHash        [sha256.Size]byte
	flowID           string
	returnPath       string
	issuedAt         time.Time
	expiresAt        time.Time
	codeHash         [sha256.Size]byte
	claimHash        [sha256.Size]byte
	claimedAt        time.Time
	closedAt         time.Time
	retainUntil      time.Time
	quarantineReason string
}

func (entry onboardingStateEntry) claim(disposition CallbackClaimDisposition, replayed bool) CallbackClaim {
	return CallbackClaim{
		binding:     OnboardingFlowBinding{FlowID: entry.flowID, ReturnPath: entry.returnPath},
		issuedAt:    entry.issuedAt,
		expiresAt:   entry.expiresAt,
		claimedAt:   entry.claimedAt,
		disposition: disposition,
		replayed:    replayed,
		stateHash:   entry.stateHash,
		codeHash:    entry.codeHash,
		claimHash:   entry.claimHash,
		flowID:      entry.flowID,
		returnPath:  entry.returnPath,
		valid:       true,
	}
}

func (entry onboardingStateEntry) matchesClaim(claim CallbackClaim) bool {
	return digestEqual(entry.stateHash, claim.stateHash) &&
		digestEqual(entry.codeHash, claim.codeHash) &&
		digestEqual(entry.claimHash, claim.claimHash) &&
		entry.flowID == claim.flowID && entry.returnPath == claim.returnPath
}

func (claim CallbackClaim) validFence() bool {
	return claim.valid && validOnboardingBinding(OnboardingFlowBinding{FlowID: claim.flowID, ReturnPath: claim.returnPath})
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidOnboardingState
	}
	return ctx.Err()
}

func onboardingStateHash(state string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	decoded, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != state {
		return zero, ErrInvalidOnboardingState
	}
	return sha256.Sum256([]byte(state)), nil
}

func validOnboardingBinding(binding OnboardingFlowBinding) bool {
	if len(binding.FlowID) == 0 || len(binding.FlowID) > maxOnboardingFlowIDBytes {
		return false
	}
	for _, character := range binding.FlowID {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_') {
			return false
		}
	}
	if len(binding.ReturnPath) == 0 || len(binding.ReturnPath) > maxOnboardingReturnPath || !utf8.ValidString(binding.ReturnPath) || binding.ReturnPath[0] != '/' || strings.HasPrefix(binding.ReturnPath, "//") || strings.Contains(binding.ReturnPath, "\\") {
		return false
	}
	for _, character := range binding.ReturnPath {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	parsed, err := url.ParseRequestURI(binding.ReturnPath)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") || strings.Contains(parsed.Path, "\\") {
		return false
	}
	for _, character := range parsed.Path {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validOnboardingClaimID(claimID string) bool {
	if len(claimID) == 0 || len(claimID) > maxOnboardingClaimIDBytes {
		return false
	}
	for _, character := range []byte(claimID) {
		if character <= 0x20 || character >= 0x7f {
			return false
		}
	}
	return true
}

func validCallbackQuarantineReason(reason CallbackQuarantineReason) bool {
	switch reason {
	case CallbackQuarantineExchangeAmbiguous, CallbackQuarantineReconcileAmbiguous, CallbackQuarantineCoordinatorAborted:
		return true
	default:
		return false
	}
}

func validUTC(value time.Time) bool {
	if value.IsZero() || value.Location() != time.UTC {
		return false
	}
	canonical, err := time.Parse(time.RFC3339Nano, value.Format(time.RFC3339Nano))
	return err == nil && canonical.Equal(value)
}

func validOnboardingInterval(now, expiresAt time.Time) bool {
	return validUTC(now) && validUTC(expiresAt) && expiresAt.After(now) && expiresAt.Sub(now) <= maxOnboardingStateLifetime
}

func digestEqual(left, right [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

// transact prunes time-expired states and runs fn in one immediate SQLite
// transaction. fn returns a protocol outcome only before writing anything, so
// such an outcome still commits the prune. Any other error, or a ctx that
// ended, rolls back; storage failures are reported as the redacted
// ErrOnboardingStateStoreIO.
func (store *OnboardingStateStore) transact(ctx context.Context, now time.Time, fn func(*sql.Tx) error) error {
	if store == nil || store.db == nil {
		return ErrOnboardingStateStoreIO
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return onboardingStoreFailure(ctx)
	}
	if err := pruneOnboardingStates(tx, now); err != nil {
		_ = tx.Rollback()
		return onboardingStoreFailure(ctx)
	}
	outcome := fn(tx)
	if outcome != nil && !isOnboardingOutcome(outcome) {
		_ = tx.Rollback()
		return onboardingStoreFailure(ctx)
	}
	if err := contextError(ctx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return onboardingStoreFailure(ctx)
	}
	return outcome
}

func isOnboardingOutcome(err error) bool {
	for _, outcome := range []error{ErrOnboardingStateConflict, ErrOnboardingStateLimit,
		ErrOnboardingStateRejected, ErrOnboardingStateRecoveryRequired} {
		if errors.Is(err, outcome) {
			return true
		}
	}
	return false
}

// onboardingStoreFailure reports cancellation as itself and hides every other
// (storage) error behind ErrOnboardingStateStoreIO.
func onboardingStoreFailure(ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return ErrOnboardingStateStoreIO
}

// pruneOnboardingStates drops expired pending states and expired tombstones,
// and quarantines claims that expired before closing; they stay fenced through
// the replay window.
func pruneOnboardingStates(tx *sql.Tx, now time.Time) error {
	at, window := now.UnixNano(), int64(maxOnboardingReplayWindow)
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM onboarding_states WHERE status='pending' AND expires_at<=?`, []any{at}},
		{`DELETE FROM onboarding_states WHERE status='claimed' AND expires_at+?<=?`, []any{window, at}},
		{`UPDATE onboarding_states SET status='quarantined',closed_at=expires_at,retain_until=expires_at+?,quarantine_reason=?
WHERE status='claimed' AND expires_at<=?`, []any{window, quarantineReasonClaimExpired, at}},
		{`DELETE FROM onboarding_states WHERE status IN ('completed','quarantined') AND retain_until<=?`, []any{at}},
	} {
		if _, err := tx.Exec(statement.sql, statement.args...); err != nil {
			return err
		}
	}
	return nil
}

const onboardingStateColumns = `status,state_sha256,flow_id,return_path,issued_at,expires_at,
code_sha256,claim_sha256,claimed_at,closed_at,retain_until,quarantine_reason`

// loadOnboardingState reads one state by digest; a missing state is rejected.
func loadOnboardingState(tx *sql.Tx, stateHash [sha256.Size]byte) (onboardingStateEntry, error) {
	entry, err := scanOnboardingState(tx.QueryRow(`SELECT `+onboardingStateColumns+` FROM onboarding_states WHERE state_sha256=?`, stateHash[:]))
	if errors.Is(err, sql.ErrNoRows) {
		return onboardingStateEntry{}, ErrOnboardingStateRejected
	}
	return entry, err
}

func scanOnboardingState(row interface{ Scan(...any) error }) (onboardingStateEntry, error) {
	var entry onboardingStateEntry
	var stateHash, codeHash, claimHash []byte
	var issuedAt, expiresAt int64
	var claimedAt, closedAt, retainUntil sql.NullInt64
	var reason sql.NullString
	if err := row.Scan(&entry.status, &stateHash, &entry.flowID, &entry.returnPath, &issuedAt, &expiresAt,
		&codeHash, &claimHash, &claimedAt, &closedAt, &retainUntil, &reason); err != nil {
		return onboardingStateEntry{}, err
	}
	copy(entry.stateHash[:], stateHash)
	copy(entry.codeHash[:], codeHash)
	copy(entry.claimHash[:], claimHash)
	entry.issuedAt, entry.expiresAt = time.Unix(0, issuedAt).UTC(), time.Unix(0, expiresAt).UTC()
	for _, field := range []struct {
		value  sql.NullInt64
		target *time.Time
	}{{claimedAt, &entry.claimedAt}, {closedAt, &entry.closedAt}, {retainUntil, &entry.retainUntil}} {
		if field.value.Valid {
			*field.target = time.Unix(0, field.value.Int64).UTC()
		}
	}
	entry.quarantineReason = reason.String
	return entry, nil
}
