package githubapp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/atomicfile"
)

const (
	onboardingStateStoreVersion      = 3
	onboardingStateFileName          = "onboarding-states.json"
	maxOnboardingStateFileBytes      = 128 << 10
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
	ErrOnboardingStateStoreSecurity    = errors.New("GitHub App onboarding state store has unsafe filesystem permissions, ownership, or type")
	ErrOnboardingStateStoreIO          = errors.New("GitHub App onboarding state store operation failed")
	ErrOnboardingStateStoreInvalid     = errors.New("stored GitHub App onboarding states are invalid")
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

func (binding OnboardingFlowBinding) String() string {
	return "GitHub App onboarding flow binding"
}

func (binding OnboardingFlowBinding) GoString() string {
	return binding.String()
}

// CallbackClaimDisposition tells the callback coordinator what it may do next.
// Only CallbackClaimExchangeOnce authorizes a manifest exchange.
type CallbackClaimDisposition string

const (
	CallbackClaimExchangeOnce  CallbackClaimDisposition = "exchange_once"
	CallbackClaimReconcileOnly CallbackClaimDisposition = "reconcile_only"
)

func (disposition CallbackClaimDisposition) String() string {
	switch disposition {
	case CallbackClaimExchangeOnce, CallbackClaimReconcileOnly:
		return string(disposition)
	default:
		return "invalid_callback_claim_disposition"
	}
}

func (disposition CallbackClaimDisposition) GoString() string {
	return disposition.String()
}

// CallbackQuarantineReason is a bounded, non-sensitive reason for closing an
// ambiguous callback claim.
type CallbackQuarantineReason string

const (
	CallbackQuarantineExchangeAmbiguous  CallbackQuarantineReason = "exchange_ambiguous"
	CallbackQuarantineReconcileAmbiguous CallbackQuarantineReason = "reconcile_ambiguous"
	CallbackQuarantineCoordinatorAborted CallbackQuarantineReason = "coordinator_aborted"
)

func (reason CallbackQuarantineReason) String() string {
	if validCallbackQuarantineReason(reason) {
		return string(reason)
	}
	return "invalid_callback_quarantine_reason"
}

func (reason CallbackQuarantineReason) GoString() string {
	return reason.String()
}

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

func (claim CallbackClaim) String() string {
	return "GitHub App onboarding callback claim"
}

func (claim CallbackClaim) GoString() string {
	return claim.String()
}

// OnboardingStateStore persists bounded, one-use callback states in a
// caller-owned private directory. Raw state, callback code, and claim ID values
// are never persisted. Transactions are serialized per store; Fern opens one
// store per process.
type OnboardingStateStore struct {
	directory   string
	transaction chan struct{}
}

// NewOnboardingStateStore creates or validates the private state directory.
func NewOnboardingStateStore(directory string) (*OnboardingStateStore, error) {
	if directory == "" {
		return nil, ErrOnboardingStateStoreSecurity
	}
	if err := atomicfile.PrivateDir(directory); err != nil {
		if errors.Is(err, atomicfile.ErrUnsafeDir) {
			return nil, ErrOnboardingStateStoreSecurity
		}
		return nil, ErrOnboardingStateStoreIO
	}
	return &OnboardingStateStore{directory: directory, transaction: make(chan struct{}, 1)}, nil
}

func (store *OnboardingStateStore) String() string {
	return "GitHub App onboarding state store"
}

func (store *OnboardingStateStore) GoString() string {
	return store.String()
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
	if store == nil || store.directory == "" {
		return ErrOnboardingStateStoreSecurity
	}

	if err := store.lock(ctx); err != nil {
		return err
	}
	defer store.unlock()

	entries, _, err := store.read()
	if err != nil {
		return err
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := store.write(ctx, entries); err != nil {
			return err
		}
	}
	active := 0
	for i := range entries {
		if digestEqual(entries[i].stateHash, stateHash) {
			return ErrOnboardingStateConflict
		}
		if entries[i].active() {
			active++
			if entries[i].flowID == binding.FlowID {
				return ErrOnboardingStateConflict
			}
		}
	}
	if len(entries) >= maxOnboardingStates || active >= maxOnboardingActiveStates {
		return ErrOnboardingStateLimit
	}
	entries = append(entries, onboardingStateEntry{
		status:     onboardingStateStatusPending,
		stateHash:  stateHash,
		flowID:     binding.FlowID,
		returnPath: binding.ReturnPath,
		issuedAt:   now,
		expiresAt:  expiresAt,
	})
	return store.write(ctx, entries)
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
	if store == nil || store.directory == "" {
		return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateStoreSecurity
	}
	if err := store.lock(ctx); err != nil {
		return OnboardingFlowBinding{}, time.Time{}, err
	}
	defer store.unlock()
	entries, exists, err := store.read()
	if err != nil {
		return OnboardingFlowBinding{}, time.Time{}, err
	}
	if !exists {
		return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateRejected
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := store.write(ctx, entries); err != nil {
			return OnboardingFlowBinding{}, time.Time{}, err
		}
	}
	for i := range entries {
		entry := entries[i]
		if digestEqual(entry.stateHash, stateHash) {
			if entry.status != onboardingStateStatusPending || now.Before(entry.issuedAt) || !entry.expiresAt.After(now) {
				return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateRecoveryRequired
			}
			binding := OnboardingFlowBinding{FlowID: entry.flowID, ReturnPath: entry.returnPath}
			if !validOnboardingBinding(binding) {
				return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateStoreInvalid
			}
			return binding, entry.expiresAt, nil
		}
	}
	return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateRejected
}

// Claim durably fences a pending callback before any manifest exchange. An
// exact retry receives the same fence with reconcile-only disposition. A
// changed state binding, code digest, or claim ID is rejected identically.
func (store *OnboardingStateStore) Claim(ctx context.Context, state string, binding OnboardingFlowBinding, callbackCodeDigest [sha256.Size]byte, claimID string, now time.Time) (CallbackClaim, error) {
	var zero CallbackClaim
	if err := contextError(ctx); err != nil {
		return zero, err
	}
	stateHash, err := onboardingStateHash(state)
	if err != nil || !validOnboardingBinding(binding) || !validOnboardingClaimID(claimID) || callbackCodeDigest == ([sha256.Size]byte{}) || !validUTC(now) {
		return zero, ErrOnboardingStateRejected
	}
	if store == nil || store.directory == "" {
		return zero, ErrOnboardingStateStoreSecurity
	}
	claimHash := sha256.Sum256([]byte(claimID))

	if err := store.lock(ctx); err != nil {
		return zero, err
	}
	defer store.unlock()

	entries, exists, err := store.read()
	if err != nil {
		return zero, err
	}
	if !exists {
		return zero, ErrOnboardingStateRejected
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := store.write(ctx, entries); err != nil {
			return zero, err
		}
	}
	matched := -1
	for i := range entries {
		if digestEqual(entries[i].stateHash, stateHash) {
			matched = i
		}
	}
	if matched < 0 {
		return zero, ErrOnboardingStateRejected
	}
	entry := &entries[matched]
	fenceMatches := entry.flowID == binding.FlowID && entry.returnPath == binding.ReturnPath
	if now.Before(entry.issuedAt) {
		return zero, ErrOnboardingStateRejected
	}
	switch entry.status {
	case onboardingStateStatusPending:
		if !fenceMatches || !entry.expiresAt.After(now) || digestInUse(entries, callbackCodeDigest, claimHash, matched) {
			return zero, ErrOnboardingStateRejected
		}
		entry.status = onboardingStateStatusClaimed
		entry.codeHash = callbackCodeDigest
		entry.claimHash = claimHash
		entry.claimedAt = now
		if err := store.write(ctx, entries); err != nil {
			return zero, err
		}
		return entry.claim(CallbackClaimExchangeOnce, false), nil
	case onboardingStateStatusClaimed:
		if !fenceMatches || !digestEqual(entry.codeHash, callbackCodeDigest) || !digestEqual(entry.claimHash, claimHash) {
			return zero, ErrOnboardingStateRejected
		}
		return entry.claim(CallbackClaimReconcileOnly, true), nil
	case onboardingStateStatusCompleted, onboardingStateStatusQuarantined:
		if fenceMatches && digestEqual(entry.codeHash, callbackCodeDigest) && digestEqual(entry.claimHash, claimHash) {
			return zero, ErrOnboardingStateRecoveryRequired
		}
		return zero, ErrOnboardingStateRejected
	default:
		return zero, ErrOnboardingStateStoreInvalid
	}
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
	if store == nil || store.directory == "" {
		return ErrOnboardingStateStoreSecurity
	}
	if err := store.lock(ctx); err != nil {
		return err
	}
	defer store.unlock()

	entries, exists, err := store.read()
	if err != nil {
		return err
	}
	if !exists {
		return ErrOnboardingStateRejected
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := store.write(ctx, entries); err != nil {
			return err
		}
	}
	matched := -1
	for i := range entries {
		if digestEqual(entries[i].stateHash, claim.stateHash) {
			matched = i
		}
	}
	if matched < 0 {
		return ErrOnboardingStateRejected
	}
	entry := &entries[matched]
	if !entry.matchesClaim(claim) {
		return ErrOnboardingStateRejected
	}
	if reason == "" && entry.status == onboardingStateStatusCompleted {
		if now.Before(entry.closedAt) {
			return ErrOnboardingStateRejected
		}
		return nil
	}
	if reason != "" && entry.status == onboardingStateStatusQuarantined && entry.quarantineReason == reason {
		if now.Before(entry.closedAt) {
			return ErrOnboardingStateRejected
		}
		return nil
	}
	if entry.status != onboardingStateStatusClaimed {
		return ErrOnboardingStateRecoveryRequired
	}
	if now.Before(entry.claimedAt) {
		return ErrOnboardingStateRejected
	}
	if !entry.expiresAt.After(now) {
		return ErrOnboardingStateRecoveryRequired
	}
	entry.closedAt = now
	entry.retainUntil = entry.expiresAt.Add(maxOnboardingReplayWindow)
	if reason == "" {
		entry.status = onboardingStateStatusCompleted
	} else {
		entry.status = onboardingStateStatusQuarantined
		entry.quarantineReason = reason
	}
	return store.write(ctx, entries)
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

func (entry onboardingStateEntry) active() bool {
	return entry.status == onboardingStateStatusPending || entry.status == onboardingStateStatusClaimed
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

// onboardingStateFile is the on-disk form. Digests are hex encoded; raw state,
// callback code, and claim ID values are never stored.
type onboardingStateFile struct {
	Version int                          `json:"version"`
	Entries []storedOnboardingStateEntry `json:"entries"`
}

type storedOnboardingStateEntry struct {
	Status           string    `json:"status"`
	StateHash        string    `json:"state_sha256"`
	FlowID           string    `json:"flow_id"`
	ReturnPath       string    `json:"return_path"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	CodeHash         string    `json:"callback_code_sha256,omitempty"`
	ClaimHash        string    `json:"claim_id_sha256,omitempty"`
	ClaimedAt        time.Time `json:"claimed_at,omitzero"`
	ClosedAt         time.Time `json:"closed_at,omitzero"`
	RetainUntil      time.Time `json:"retain_until,omitzero"`
	QuarantineReason string    `json:"quarantine_reason,omitempty"`
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidOnboardingState
	}
	return ctx.Err()
}

// lock acquires the store's transaction slot, giving up if ctx ends first.
func (store *OnboardingStateStore) lock(ctx context.Context) error {
	select {
	case store.transaction <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (store *OnboardingStateStore) unlock() {
	<-store.transaction
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

func digestInUse(entries []onboardingStateEntry, codeHash, claimHash [sha256.Size]byte, except int) bool {
	for i := range entries {
		if i != except && entries[i].status != onboardingStateStatusPending && (digestEqual(entries[i].codeHash, codeHash) || digestEqual(entries[i].claimHash, claimHash)) {
			return true
		}
	}
	return false
}

func pruneOnboardingStates(entries []onboardingStateEntry, now time.Time) ([]onboardingStateEntry, bool) {
	kept := entries[:0]
	changed := false
	for _, entry := range entries {
		switch entry.status {
		case onboardingStateStatusPending:
			if !entry.expiresAt.After(now) {
				changed = true
				continue
			}
		case onboardingStateStatusClaimed:
			if !entry.expiresAt.After(now) {
				retainUntil := entry.expiresAt.Add(maxOnboardingReplayWindow)
				if !retainUntil.After(now) {
					changed = true
					continue
				}
				entry.status = onboardingStateStatusQuarantined
				entry.closedAt = entry.expiresAt
				entry.retainUntil = retainUntil
				entry.quarantineReason = quarantineReasonClaimExpired
				changed = true
			}
		case onboardingStateStatusCompleted, onboardingStateStatusQuarantined:
			if !entry.retainUntil.After(now) {
				changed = true
				continue
			}
		}
		kept = append(kept, entry)
	}
	return kept, changed
}

func (store *OnboardingStateStore) path() string {
	return filepath.Join(store.directory, onboardingStateFileName)
}

func (store *OnboardingStateStore) read() ([]onboardingStateEntry, bool, error) {
	payload, err := atomicfile.Read(store.path(), maxOnboardingStateFileBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case errors.Is(err, atomicfile.ErrTooLarge):
		return nil, false, ErrOnboardingStateStoreInvalid
	case err != nil:
		return nil, false, ErrOnboardingStateStoreIO
	}
	entries, err := decodeOnboardingStateFile(payload)
	if err != nil {
		return nil, false, ErrOnboardingStateStoreInvalid
	}
	return entries, true, nil
}

func (store *OnboardingStateStore) write(ctx context.Context, entries []onboardingStateEntry) error {
	payload, err := encodeOnboardingStateFile(entries)
	if err != nil || len(payload) > maxOnboardingStateFileBytes {
		return ErrOnboardingStateStoreInvalid
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := atomicfile.Write(store.path(), payload, 0o600); err != nil {
		return ErrOnboardingStateStoreIO
	}
	return nil
}

func encodeOnboardingStateFile(entries []onboardingStateEntry) ([]byte, error) {
	if len(entries) > maxOnboardingStates {
		return nil, ErrOnboardingStateStoreInvalid
	}
	file := onboardingStateFile{Version: onboardingStateStoreVersion, Entries: make([]storedOnboardingStateEntry, len(entries))}
	for i, entry := range entries {
		stored := storedOnboardingStateEntry{
			Status:           entry.status,
			StateHash:        hex.EncodeToString(entry.stateHash[:]),
			FlowID:           entry.flowID,
			ReturnPath:       entry.returnPath,
			IssuedAt:         entry.issuedAt,
			ExpiresAt:        entry.expiresAt,
			ClaimedAt:        entry.claimedAt,
			ClosedAt:         entry.closedAt,
			RetainUntil:      entry.retainUntil,
			QuarantineReason: entry.quarantineReason,
		}
		if entry.status != onboardingStateStatusPending {
			stored.CodeHash = hex.EncodeToString(entry.codeHash[:])
			stored.ClaimHash = hex.EncodeToString(entry.claimHash[:])
		}
		file.Entries[i] = stored
	}
	return json.Marshal(file)
}

// decodeOnboardingStateFile loads Fern's own state file. It checks the schema
// version and that every digest and status is usable; expired entries are
// pruned by each transaction after loading.
func decodeOnboardingStateFile(payload []byte) ([]onboardingStateEntry, error) {
	var file onboardingStateFile
	if err := json.Unmarshal(payload, &file); err != nil || file.Version != onboardingStateStoreVersion || len(file.Entries) > maxOnboardingStates {
		return nil, ErrOnboardingStateStoreInvalid
	}
	entries := make([]onboardingStateEntry, len(file.Entries))
	for i, stored := range file.Entries {
		switch stored.Status {
		case onboardingStateStatusPending, onboardingStateStatusClaimed, onboardingStateStatusCompleted, onboardingStateStatusQuarantined:
		default:
			return nil, ErrOnboardingStateStoreInvalid
		}
		entry := onboardingStateEntry{
			status:           stored.Status,
			flowID:           stored.FlowID,
			returnPath:       stored.ReturnPath,
			issuedAt:         stored.IssuedAt,
			expiresAt:        stored.ExpiresAt,
			claimedAt:        stored.ClaimedAt,
			closedAt:         stored.ClosedAt,
			retainUntil:      stored.RetainUntil,
			quarantineReason: stored.QuarantineReason,
		}
		ok := decodeDigest(stored.StateHash, &entry.stateHash)
		if stored.Status != onboardingStateStatusPending {
			ok = ok && decodeDigest(stored.CodeHash, &entry.codeHash) && decodeDigest(stored.ClaimHash, &entry.claimHash)
		}
		if !ok {
			return nil, ErrOnboardingStateStoreInvalid
		}
		entries[i] = entry
	}
	return entries, nil
}

func decodeDigest(value string, digest *[sha256.Size]byte) bool {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	copy(digest[:], decoded)
	return true
}
