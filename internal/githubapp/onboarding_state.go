package githubapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	onboardingStateStoreVersion      = 2
	onboardingStateFileName          = "onboarding-states-v2.json"
	onboardingStateTempStem          = ".onboarding-states-v2-"
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

	onboardingStateTransaction  = make(chan struct{}, 1)
	onboardingStateTempSequence atomic.Uint64
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
// are never persisted.
type OnboardingStateStore struct {
	directory string
}

// NewOnboardingStateStore creates or validates the private state directory.
func NewOnboardingStateStore(directory string) (*OnboardingStateStore, error) {
	if directory == "" {
		return nil, ErrOnboardingStateStoreSecurity
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, ErrOnboardingStateStoreIO
	}
	directoryHandle, err := openOnboardingStateDirectory(directory)
	if err != nil {
		return nil, err
	}
	if err := directoryHandle.Close(); err != nil {
		return nil, ErrOnboardingStateStoreIO
	}
	return &OnboardingStateStore{directory: directory}, nil
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

	if err := lockOnboardingStateTransaction(ctx); err != nil {
		return err
	}
	defer unlockOnboardingStateTransaction()

	directory, err := openOnboardingStateDirectory(store.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	entries, _, err := readOnboardingStates(int(directory.Fd()))
	if err != nil {
		return err
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := writeOnboardingStates(ctx, directory, entries); err != nil {
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
	return writeOnboardingStates(ctx, directory, entries)
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
	if err := lockOnboardingStateTransaction(ctx); err != nil {
		return OnboardingFlowBinding{}, time.Time{}, err
	}
	defer unlockOnboardingStateTransaction()
	directory, err := openOnboardingStateDirectory(store.directory)
	if err != nil {
		return OnboardingFlowBinding{}, time.Time{}, err
	}
	defer directory.Close()
	entries, exists, err := readOnboardingStates(int(directory.Fd()))
	if err != nil {
		return OnboardingFlowBinding{}, time.Time{}, err
	}
	if !exists {
		return OnboardingFlowBinding{}, time.Time{}, ErrOnboardingStateRejected
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := writeOnboardingStates(ctx, directory, entries); err != nil {
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

	if err := lockOnboardingStateTransaction(ctx); err != nil {
		return zero, err
	}
	defer unlockOnboardingStateTransaction()

	directory, err := openOnboardingStateDirectory(store.directory)
	if err != nil {
		return zero, err
	}
	defer directory.Close()
	entries, exists, err := readOnboardingStates(int(directory.Fd()))
	if err != nil {
		return zero, err
	}
	if !exists {
		return zero, ErrOnboardingStateRejected
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := writeOnboardingStates(ctx, directory, entries); err != nil {
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
		if err := writeOnboardingStates(ctx, directory, entries); err != nil {
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
	if err := lockOnboardingStateTransaction(ctx); err != nil {
		return err
	}
	defer unlockOnboardingStateTransaction()

	directory, err := openOnboardingStateDirectory(store.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	entries, exists, err := readOnboardingStates(int(directory.Fd()))
	if err != nil {
		return err
	}
	if !exists {
		return ErrOnboardingStateRejected
	}
	entries, pruned := pruneOnboardingStates(entries, now)
	if pruned {
		if err := writeOnboardingStates(ctx, directory, entries); err != nil {
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
	return writeOnboardingStates(ctx, directory, entries)
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

type storedOnboardingStateEntry struct {
	Status           string `json:"status"`
	StateHash        string `json:"state_sha256"`
	FlowID           string `json:"flow_id"`
	ReturnPath       string `json:"return_path"`
	IssuedAt         string `json:"issued_at"`
	ExpiresAt        string `json:"expires_at"`
	CodeHash         string `json:"callback_code_sha256,omitempty"`
	ClaimHash        string `json:"claim_id_sha256,omitempty"`
	ClaimedAt        string `json:"claimed_at,omitempty"`
	ClosedAt         string `json:"closed_at,omitempty"`
	RetainUntil      string `json:"retain_until,omitempty"`
	QuarantineReason string `json:"quarantine_reason,omitempty"`
}

type unsignedOnboardingStateFile struct {
	Version int                          `json:"version"`
	Entries []storedOnboardingStateEntry `json:"entries"`
}

type storedOnboardingStateFile struct {
	Version  int                          `json:"version"`
	Entries  []storedOnboardingStateEntry `json:"entries"`
	Checksum string                       `json:"checksum"`
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidOnboardingState
	}
	return ctx.Err()
}

func lockOnboardingStateTransaction(ctx context.Context) error {
	select {
	case onboardingStateTransaction <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unlockOnboardingStateTransaction() {
	<-onboardingStateTransaction
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

func openOnboardingStateDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, ErrOnboardingStateStoreSecurity
		}
		return nil, ErrOnboardingStateStoreIO
	}
	handle := os.NewFile(uintptr(fd), "GitHub App onboarding state directory")
	if handle == nil {
		unix.Close(fd)
		return nil, ErrOnboardingStateStoreIO
	}
	info, err := handle.Stat()
	var stat unix.Stat_t
	if err != nil || unix.Fstat(fd, &stat) != nil {
		handle.Close()
		return nil, ErrOnboardingStateStoreIO
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || stat.Uid != uint32(unix.Geteuid()) {
		handle.Close()
		return nil, ErrOnboardingStateStoreSecurity
	}
	return handle, nil
}

func inspectOnboardingStateEntry(directoryFD int) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(directoryFD, onboardingStateFileName, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, ErrOnboardingStateStoreIO
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Nlink != 1 || stat.Uid != uint32(unix.Geteuid()) {
		return false, ErrOnboardingStateStoreSecurity
	}
	return true, nil
}

func requirePrivateOnboardingStateFile(file *os.File) error {
	info, err := file.Stat()
	var stat unix.Stat_t
	if err != nil || unix.Fstat(int(file.Fd()), &stat) != nil {
		return ErrOnboardingStateStoreIO
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || stat.Nlink > 1 || stat.Uid != uint32(unix.Geteuid()) {
		return ErrOnboardingStateStoreSecurity
	}
	return nil
}

func readOnboardingStates(directoryFD int) ([]onboardingStateEntry, bool, error) {
	exists, err := inspectOnboardingStateEntry(directoryFD)
	if err != nil || !exists {
		return nil, exists, err
	}
	fd, err := unix.Openat(directoryFD, onboardingStateFileName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, ErrOnboardingStateStoreIO
	}
	file := os.NewFile(uintptr(fd), onboardingStateFileName)
	if file == nil {
		unix.Close(fd)
		return nil, false, ErrOnboardingStateStoreIO
	}
	defer file.Close()
	if err := requirePrivateOnboardingStateFile(file); err != nil {
		return nil, false, err
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxOnboardingStateFileBytes+1))
	if err != nil {
		return nil, false, ErrOnboardingStateStoreIO
	}
	if len(payload) > maxOnboardingStateFileBytes {
		return nil, false, ErrOnboardingStateStoreInvalid
	}
	entries, err := decodeOnboardingStateFile(payload)
	if err != nil {
		return nil, false, ErrOnboardingStateStoreInvalid
	}
	return entries, true, nil
}

func writeOnboardingStates(ctx context.Context, directory *os.File, entries []onboardingStateEntry) error {
	payload, err := encodeOnboardingStateFile(entries)
	if err != nil || len(payload) > maxOnboardingStateFileBytes {
		return ErrOnboardingStateStoreInvalid
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if _, err := inspectOnboardingStateEntry(int(directory.Fd())); err != nil {
		return err
	}
	temporary, temporaryName, err := createOnboardingStateTemporary(int(directory.Fd()))
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(directory.Fd()), temporaryName, 0)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return ErrOnboardingStateStoreIO
	}
	if err := requirePrivateOnboardingStateFile(temporary); err != nil {
		temporary.Close()
		return err
	}
	if written, err := temporary.Write(payload); err != nil || written != len(payload) {
		temporary.Close()
		return ErrOnboardingStateStoreIO
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return ErrOnboardingStateStoreIO
	}
	if err := temporary.Close(); err != nil {
		return ErrOnboardingStateStoreIO
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if _, err := inspectOnboardingStateEntry(int(directory.Fd())); err != nil {
		return err
	}
	if err := unix.Renameat(int(directory.Fd()), temporaryName, int(directory.Fd()), onboardingStateFileName); err != nil {
		return ErrOnboardingStateStoreIO
	}
	if err := directory.Sync(); err != nil {
		return ErrOnboardingStateStoreIO
	}
	return nil
}

func createOnboardingStateTemporary(directoryFD int) (*os.File, string, error) {
	for range 100 {
		sequence := onboardingStateTempSequence.Add(1)
		name := fmt.Sprintf("%s%d-%d.tmp", onboardingStateTempStem, os.Getpid(), sequence)
		fd, err := unix.Openat(directoryFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", ErrOnboardingStateStoreIO
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			unix.Close(fd)
			return nil, "", ErrOnboardingStateStoreIO
		}
		return file, name, nil
	}
	return nil, "", ErrOnboardingStateStoreIO
}

func encodeOnboardingStateFile(entries []onboardingStateEntry) ([]byte, error) {
	if len(entries) > maxOnboardingStates {
		return nil, ErrOnboardingStateStoreInvalid
	}
	ordered := append([]onboardingStateEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool {
		return bytes.Compare(ordered[i].stateHash[:], ordered[j].stateHash[:]) < 0
	})
	storedEntries := make([]storedOnboardingStateEntry, len(ordered))
	for i, entry := range ordered {
		storedEntries[i] = storeOnboardingStateEntry(entry)
	}
	payload, err := marshalOnboardingStateFile(storedEntries)
	if err != nil {
		return nil, err
	}
	if _, err := decodeOnboardingStateFile(payload); err != nil {
		return nil, ErrOnboardingStateStoreInvalid
	}
	return payload, nil
}

func storeOnboardingStateEntry(entry onboardingStateEntry) storedOnboardingStateEntry {
	stored := storedOnboardingStateEntry{
		Status:     entry.status,
		StateHash:  hex.EncodeToString(entry.stateHash[:]),
		FlowID:     entry.flowID,
		ReturnPath: entry.returnPath,
		IssuedAt:   entry.issuedAt.Format(time.RFC3339Nano),
		ExpiresAt:  entry.expiresAt.Format(time.RFC3339Nano),
	}
	if entry.status != onboardingStateStatusPending {
		stored.CodeHash = hex.EncodeToString(entry.codeHash[:])
		stored.ClaimHash = hex.EncodeToString(entry.claimHash[:])
		stored.ClaimedAt = entry.claimedAt.Format(time.RFC3339Nano)
	}
	if entry.status == onboardingStateStatusCompleted || entry.status == onboardingStateStatusQuarantined {
		stored.ClosedAt = entry.closedAt.Format(time.RFC3339Nano)
		stored.RetainUntil = entry.retainUntil.Format(time.RFC3339Nano)
	}
	if entry.status == onboardingStateStatusQuarantined {
		stored.QuarantineReason = entry.quarantineReason
	}
	return stored
}

func marshalOnboardingStateFile(entries []storedOnboardingStateEntry) ([]byte, error) {
	unsigned := unsignedOnboardingStateFile{Version: onboardingStateStoreVersion, Entries: entries}
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		return nil, err
	}
	checksum := sha256.Sum256(canonical)
	payload, err := json.Marshal(storedOnboardingStateFile{
		Version:  unsigned.Version,
		Entries:  unsigned.Entries,
		Checksum: hex.EncodeToString(checksum[:]),
	})
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func decodeOnboardingStateFile(payload []byte) ([]onboardingStateEntry, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrOnboardingStateStoreInvalid
	}
	var version int
	var rawEntries []json.RawMessage
	var checksum string
	seen := make(map[string]bool, 3)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return nil, ErrOnboardingStateStoreInvalid
		}
		seen[name] = true
		switch name {
		case "version":
			err = decoder.Decode(&version)
		case "entries":
			err = decoder.Decode(&rawEntries)
		case "checksum":
			err = decoder.Decode(&checksum)
		default:
			return nil, ErrOnboardingStateStoreInvalid
		}
		if err != nil {
			return nil, ErrOnboardingStateStoreInvalid
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 3 || version != onboardingStateStoreVersion || rawEntries == nil || len(rawEntries) > maxOnboardingStates {
		return nil, ErrOnboardingStateStoreInvalid
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, ErrOnboardingStateStoreInvalid
	}

	storedEntries := make([]storedOnboardingStateEntry, len(rawEntries))
	entries := make([]onboardingStateEntry, len(rawEntries))
	stateHashes := make(map[[sha256.Size]byte]bool, len(rawEntries))
	codeHashes := make(map[[sha256.Size]byte]bool, len(rawEntries))
	claimHashes := make(map[[sha256.Size]byte]bool, len(rawEntries))
	activeFlows := make(map[string]bool, len(rawEntries))
	active := 0
	for i, rawEntry := range rawEntries {
		stored, fields, err := decodeOnboardingStateEntry(rawEntry)
		if err != nil {
			return nil, ErrOnboardingStateStoreInvalid
		}
		storedEntries[i] = stored
		entry, err := validateStoredOnboardingStateEntry(stored, fields)
		if err != nil {
			return nil, ErrOnboardingStateStoreInvalid
		}
		entries[i] = entry
		if stateHashes[entry.stateHash] || (i > 0 && bytes.Compare(entries[i-1].stateHash[:], entry.stateHash[:]) >= 0) {
			return nil, ErrOnboardingStateStoreInvalid
		}
		stateHashes[entry.stateHash] = true
		if entry.status != onboardingStateStatusPending {
			if codeHashes[entry.codeHash] || claimHashes[entry.claimHash] {
				return nil, ErrOnboardingStateStoreInvalid
			}
			codeHashes[entry.codeHash] = true
			claimHashes[entry.claimHash] = true
		}
		if entry.active() {
			active++
			if activeFlows[entry.flowID] {
				return nil, ErrOnboardingStateStoreInvalid
			}
			activeFlows[entry.flowID] = true
		}
	}
	if active > maxOnboardingActiveStates {
		return nil, ErrOnboardingStateStoreInvalid
	}
	canonical, err := marshalOnboardingStateFile(storedEntries)
	if err != nil || !bytes.Equal(canonical, payload) {
		return nil, ErrOnboardingStateStoreInvalid
	}
	unsigned, err := json.Marshal(unsignedOnboardingStateFile{Version: version, Entries: storedEntries})
	if err != nil {
		return nil, ErrOnboardingStateStoreInvalid
	}
	wantChecksum := sha256.Sum256(unsigned)
	checksumBytes, err := hex.DecodeString(checksum)
	if err != nil || len(checksumBytes) != sha256.Size || hex.EncodeToString(checksumBytes) != checksum || subtle.ConstantTimeCompare(checksumBytes, wantChecksum[:]) != 1 {
		return nil, ErrOnboardingStateStoreInvalid
	}
	return entries, nil
}

func decodeOnboardingStateEntry(payload []byte) (storedOnboardingStateEntry, map[string]bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return storedOnboardingStateEntry{}, nil, ErrOnboardingStateStoreInvalid
	}
	var entry storedOnboardingStateEntry
	seen := make(map[string]bool, 12)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return storedOnboardingStateEntry{}, nil, ErrOnboardingStateStoreInvalid
		}
		seen[name] = true
		switch name {
		case "status":
			err = decoder.Decode(&entry.Status)
		case "state_sha256":
			err = decoder.Decode(&entry.StateHash)
		case "flow_id":
			err = decoder.Decode(&entry.FlowID)
		case "return_path":
			err = decoder.Decode(&entry.ReturnPath)
		case "issued_at":
			err = decoder.Decode(&entry.IssuedAt)
		case "expires_at":
			err = decoder.Decode(&entry.ExpiresAt)
		case "callback_code_sha256":
			err = decoder.Decode(&entry.CodeHash)
		case "claim_id_sha256":
			err = decoder.Decode(&entry.ClaimHash)
		case "claimed_at":
			err = decoder.Decode(&entry.ClaimedAt)
		case "closed_at":
			err = decoder.Decode(&entry.ClosedAt)
		case "retain_until":
			err = decoder.Decode(&entry.RetainUntil)
		case "quarantine_reason":
			err = decoder.Decode(&entry.QuarantineReason)
		default:
			return storedOnboardingStateEntry{}, nil, ErrOnboardingStateStoreInvalid
		}
		if err != nil {
			return storedOnboardingStateEntry{}, nil, ErrOnboardingStateStoreInvalid
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return storedOnboardingStateEntry{}, nil, ErrOnboardingStateStoreInvalid
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return storedOnboardingStateEntry{}, nil, ErrOnboardingStateStoreInvalid
	}
	return entry, seen, nil
}

func validateStoredOnboardingStateEntry(stored storedOnboardingStateEntry, fields map[string]bool) (onboardingStateEntry, error) {
	var entry onboardingStateEntry
	required := []string{"status", "state_sha256", "flow_id", "return_path", "issued_at", "expires_at"}
	for _, name := range required {
		if !fields[name] {
			return entry, ErrOnboardingStateStoreInvalid
		}
	}
	stateHash, err := decodeCanonicalDigest(stored.StateHash)
	issuedAt, issuedErr := parseCanonicalUTC(stored.IssuedAt)
	expiresAt, expiresErr := parseCanonicalUTC(stored.ExpiresAt)
	if err != nil || issuedErr != nil || expiresErr != nil || !validOnboardingInterval(issuedAt, expiresAt) || !validOnboardingBinding(OnboardingFlowBinding{FlowID: stored.FlowID, ReturnPath: stored.ReturnPath}) {
		return entry, ErrOnboardingStateStoreInvalid
	}
	entry = onboardingStateEntry{status: stored.Status, stateHash: stateHash, flowID: stored.FlowID, returnPath: stored.ReturnPath, issuedAt: issuedAt, expiresAt: expiresAt}
	claimFields := []string{"callback_code_sha256", "claim_id_sha256", "claimed_at"}
	closedFields := []string{"closed_at", "retain_until"}
	switch stored.Status {
	case onboardingStateStatusPending:
		if len(fields) != len(required) {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
		return entry, nil
	case onboardingStateStatusClaimed, onboardingStateStatusCompleted, onboardingStateStatusQuarantined:
		for _, name := range claimFields {
			if !fields[name] {
				return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
			}
		}
		entry.codeHash, err = decodeCanonicalDigest(stored.CodeHash)
		if err != nil {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
		entry.claimHash, err = decodeCanonicalDigest(stored.ClaimHash)
		if err != nil {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
		entry.claimedAt, err = parseCanonicalUTC(stored.ClaimedAt)
		if err != nil || entry.claimedAt.Before(issuedAt) || !entry.claimedAt.Before(expiresAt) {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
	}
	if stored.Status == onboardingStateStatusClaimed {
		if len(fields) != len(required)+len(claimFields) {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
		return entry, nil
	}
	for _, name := range closedFields {
		if !fields[name] {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
	}
	entry.closedAt, err = parseCanonicalUTC(stored.ClosedAt)
	if err != nil {
		return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
	}
	entry.retainUntil, err = parseCanonicalUTC(stored.RetainUntil)
	wantRetainUntil := expiresAt.Add(maxOnboardingReplayWindow)
	if err != nil || entry.closedAt.Before(entry.claimedAt) || entry.closedAt.After(expiresAt) || !entry.retainUntil.Equal(wantRetainUntil) || !entry.retainUntil.After(entry.closedAt) {
		return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
	}
	switch stored.Status {
	case onboardingStateStatusCompleted:
		if len(fields) != len(required)+len(claimFields)+len(closedFields) {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
	case onboardingStateStatusQuarantined:
		if len(fields) != len(required)+len(claimFields)+len(closedFields)+1 || !fields["quarantine_reason"] {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
		if stored.QuarantineReason != quarantineReasonClaimExpired && !validCallbackQuarantineReason(CallbackQuarantineReason(stored.QuarantineReason)) {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
		if stored.QuarantineReason == quarantineReasonClaimExpired && !entry.closedAt.Equal(expiresAt) {
			return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
		}
		entry.quarantineReason = stored.QuarantineReason
	default:
		return onboardingStateEntry{}, ErrOnboardingStateStoreInvalid
	}
	return entry, nil
}

func decodeCanonicalDigest(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != value {
		return digest, ErrOnboardingStateStoreInvalid
	}
	copy(digest[:], decoded)
	return digest, nil
}

func parseCanonicalUTC(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, ErrOnboardingStateStoreInvalid
	}
	return parsed, nil
}
