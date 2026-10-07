package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nebler/fern/internal/taskstore/taskstoretest"
)

func TestOnboardingStateStoreClaimReplayRestartAndComplete(t *testing.T) {
	store, path := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(1)
	binding := testOnboardingBinding(1)
	code := "manifest-code-secret-1"
	codeHash := testCallbackCodeDigest(code)
	claimID := "claim-secret-1"
	if err := store.Begin(context.Background(), state, binding, now, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	taskstoretest.AssertNoSecrets(t, path, state, code, claimID)

	first, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.Disposition() != CallbackClaimExchangeOnce || first.Replayed() {
		t.Fatalf("first claim = %#v, replayed = %v, disposition = %v", first, first.Replayed(), first.Disposition())
	}
	if first.Binding() != binding || !first.IssuedAt().Equal(now) || !first.ExpiresAt().Equal(now.Add(5*time.Minute)) || !first.ClaimedAt().Equal(now.Add(time.Minute)) {
		t.Fatal("first claim projection does not match durable record")
	}
	taskstoretest.AssertNoSecrets(t, path, state, code, claimID)

	restarted := NewOnboardingStateStore(store.db)
	replay, err := restarted.Claim(context.Background(), state, binding, codeHash, claimID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if replay.Disposition() != CallbackClaimReconcileOnly || !replay.Replayed() {
		t.Fatalf("replay = replayed %v, disposition %v", replay.Replayed(), replay.Disposition())
	}
	if replay.stateHash != first.stateHash || replay.codeHash != first.codeHash || replay.claimHash != first.claimHash || !replay.ClaimedAt().Equal(first.ClaimedAt()) {
		t.Fatal("replay did not return the same claim fence")
	}

	if err := restarted.Complete(context.Background(), replay, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Complete(context.Background(), replay, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("exact completion replay = %v", err)
	}
	if _, err := restarted.Claim(context.Background(), state, binding, codeHash, claimID, now.Add(4*time.Minute)); !errors.Is(err, ErrOnboardingStateRecoveryRequired) {
		t.Fatalf("completed callback replay = %v", err)
	}
	entries := readTestOnboardingEntries(t, store)
	if len(entries) != 1 || entries[0].status != onboardingStateStatusCompleted {
		t.Fatalf("entries after completion = %#v", entries)
	}
}

func TestOnboardingStateStoreClaimMismatchesAreIndistinguishable(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(2)
	binding := OnboardingFlowBinding{FlowID: "flow-secret-2", ReturnPath: "/return/path-secret-2"}
	codeHash := testCallbackCodeDigest("code-secret-2")
	claimID := "claim-secret-2"
	if err := store.Begin(context.Background(), state, binding, now, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		state   string
		binding OnboardingFlowBinding
		code    [sha256.Size]byte
		claimID string
	}{
		{name: "state", state: testOnboardingState(3), binding: binding, code: codeHash, claimID: claimID},
		{name: "flow", state: state, binding: OnboardingFlowBinding{FlowID: "flow-secret-3", ReturnPath: binding.ReturnPath}, code: codeHash, claimID: claimID},
		{name: "return path", state: state, binding: OnboardingFlowBinding{FlowID: binding.FlowID, ReturnPath: "/return/path-secret-3"}, code: codeHash, claimID: claimID},
		{name: "code", state: state, binding: binding, code: testCallbackCodeDigest("code-secret-3"), claimID: claimID},
		{name: "claim ID", state: state, binding: binding, code: codeHash, claimID: "claim-secret-3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.Claim(context.Background(), test.state, test.binding, test.code, test.claimID, now.Add(time.Minute))
			if err != ErrOnboardingStateRejected {
				t.Fatalf("mismatch error = %v", err)
			}
			assertRedacted(t, err, state, binding.FlowID, binding.ReturnPath, claimID)
		})
	}
	wrongFence := claim
	wrongFence.claimHash = testCallbackCodeDigest("different-fence")
	if err := store.Complete(context.Background(), wrongFence, now.Add(time.Minute)); err != ErrOnboardingStateRejected {
		t.Fatalf("wrong complete fence = %v", err)
	}
	if err := store.Complete(context.Background(), claim, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingStateStoreConcurrentClaimsGrantOneExchangeAuthority(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(4)
	binding := testOnboardingBinding(4)
	codeHash := testCallbackCodeDigest("code-4")
	claimID := "claim-4"
	if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	var exchange, reconcile atomic.Int32
	errorsSeen := make(chan error, 32)
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claim, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now)
			if err != nil {
				errorsSeen <- err
				return
			}
			switch claim.Disposition() {
			case CallbackClaimExchangeOnce:
				exchange.Add(1)
			case CallbackClaimReconcileOnly:
				reconcile.Add(1)
			default:
				errorsSeen <- fmt.Errorf("unexpected disposition %q", claim.Disposition())
			}
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	if exchange.Load() != 1 || reconcile.Load() != 31 {
		t.Fatalf("exchange = %d, reconcile = %d", exchange.Load(), reconcile.Load())
	}
}

func TestOnboardingStateStoreConcurrentDifferentClaimsHaveOneWinner(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(5)
	binding := testOnboardingBinding(5)
	if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var exchange, rejected atomic.Int32
	var wait sync.WaitGroup
	for i := range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claim, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest(fmt.Sprintf("code-%d", i)), fmt.Sprintf("claim-%d", i), now)
			if err == ErrOnboardingStateRejected {
				rejected.Add(1)
				return
			}
			if err == nil && claim.Disposition() == CallbackClaimExchangeOnce {
				exchange.Add(1)
			}
		}()
	}
	wait.Wait()
	if exchange.Load() != 1 || rejected.Load() != 31 {
		t.Fatalf("exchange = %d, rejected = %d", exchange.Load(), rejected.Load())
	}
}

func TestOnboardingStateStoreQuarantineIsClosedAndStable(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(6)
	binding := testOnboardingBinding(6)
	codeHash := testCallbackCodeDigest("code-6")
	claimID := "claim-6"
	if err := store.Begin(context.Background(), state, binding, now, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	claim, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(context.Background(), claim, CallbackQuarantineExchangeAmbiguous, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.Quarantine(context.Background(), claim, CallbackQuarantineExchangeAmbiguous, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("quarantine replay = %v", err)
	}
	if err := store.Quarantine(context.Background(), claim, CallbackQuarantineCoordinatorAborted, now.Add(2*time.Minute)); !errors.Is(err, ErrOnboardingStateRecoveryRequired) {
		t.Fatalf("changed quarantine reason = %v", err)
	}
	if _, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now.Add(2*time.Minute)); !errors.Is(err, ErrOnboardingStateRecoveryRequired) {
		t.Fatalf("quarantined callback = %v", err)
	}
	entry := readTestOnboardingEntries(t, store)[0]
	if entry.status != onboardingStateStatusQuarantined || entry.quarantineReason != string(CallbackQuarantineExchangeAmbiguous) {
		t.Fatalf("quarantine entry = %#v", entry)
	}
}

func TestOnboardingStateStorePendingAndClaimedExpiry(t *testing.T) {
	now := testOnboardingTime()
	t.Run("pending can be reused after expiry", func(t *testing.T) {
		store, _ := newTestOnboardingStateStore(t)
		state := testOnboardingState(7)
		binding := testOnboardingBinding(7)
		if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-7"), "claim-7", now.Add(time.Minute)); !errors.Is(err, ErrOnboardingStateRejected) {
			t.Fatalf("expired pending claim = %v", err)
		}
		later := now.Add(2 * time.Minute)
		if err := store.Begin(context.Background(), state, binding, later, later.Add(time.Minute)); err != nil {
			t.Fatalf("begin after pending expiry = %v", err)
		}
	})

	t.Run("claimed remains fenced through replay window", func(t *testing.T) {
		store, _ := newTestOnboardingStateStore(t)
		state := testOnboardingState(8)
		binding := testOnboardingBinding(8)
		codeHash := testCallbackCodeDigest("code-8")
		claimID := "claim-8"
		expiresAt := now.Add(time.Minute)
		if err := store.Begin(context.Background(), state, binding, now, expiresAt); err != nil {
			t.Fatal(err)
		}
		claim, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now)
		if err != nil {
			t.Fatal(err)
		}
		withinWindow := expiresAt.Add(time.Minute)
		if err := store.Begin(context.Background(), state, binding, withinWindow, withinWindow.Add(time.Minute)); !errors.Is(err, ErrOnboardingStateConflict) {
			t.Fatalf("claimed state reused in replay window = %v", err)
		}
		if _, err := store.Claim(context.Background(), state, binding, codeHash, claimID, withinWindow); !errors.Is(err, ErrOnboardingStateRecoveryRequired) {
			t.Fatalf("expired exact callback = %v", err)
		}
		if err := store.Complete(context.Background(), claim, withinWindow); !errors.Is(err, ErrOnboardingStateRecoveryRequired) {
			t.Fatalf("complete after claim expiry = %v", err)
		}
		entry := readTestOnboardingEntries(t, store)[0]
		if entry.status != onboardingStateStatusQuarantined || entry.quarantineReason != quarantineReasonClaimExpired {
			t.Fatalf("expired claim entry = %#v", entry)
		}
		afterWindow := expiresAt.Add(maxOnboardingReplayWindow)
		if err := store.Begin(context.Background(), state, binding, afterWindow, afterWindow.Add(time.Minute)); err != nil {
			t.Fatalf("begin after replay window = %v", err)
		}
	})
}

func TestOnboardingStateStoreCapsAndPrunesAllStatuses(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	for i := range maxOnboardingActiveStates {
		if err := store.Begin(context.Background(), testOnboardingState(byte(20+i)), testOnboardingBinding(20+i), now, now.Add(5*time.Minute)); err != nil {
			t.Fatalf("begin active %d = %v", i, err)
		}
	}
	if err := store.Begin(context.Background(), testOnboardingState(100), testOnboardingBinding(100), now, now.Add(time.Minute)); !errors.Is(err, ErrOnboardingStateLimit) {
		t.Fatalf("active cap = %v", err)
	}

	for i := range maxOnboardingActiveStates {
		state := testOnboardingState(byte(20 + i))
		binding := testOnboardingBinding(20 + i)
		claim, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest(fmt.Sprintf("cap-code-%d", i)), fmt.Sprintf("cap-claim-%d", i), now)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			err = store.Complete(context.Background(), claim, now)
		} else {
			err = store.Quarantine(context.Background(), claim, CallbackQuarantineCoordinatorAborted, now)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := maxOnboardingActiveStates; i < maxOnboardingStates; i++ {
		state := testOnboardingState(byte(20 + i))
		binding := testOnboardingBinding(20 + i)
		if err := store.Begin(context.Background(), state, binding, now, now.Add(5*time.Minute)); err != nil {
			t.Fatalf("begin tombstone %d = %v", i, err)
		}
		claim, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest(fmt.Sprintf("cap-code-%d", i)), fmt.Sprintf("cap-claim-%d", i), now)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Complete(context.Background(), claim, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(readTestOnboardingEntries(t, store)); got != maxOnboardingStates {
		t.Fatalf("entry count = %d", got)
	}
	if err := store.Begin(context.Background(), testOnboardingState(110), testOnboardingBinding(110), now, now.Add(time.Minute)); !errors.Is(err, ErrOnboardingStateLimit) {
		t.Fatalf("total cap = %v", err)
	}
	later := now.Add(5*time.Minute + maxOnboardingReplayWindow)
	if err := store.Begin(context.Background(), testOnboardingState(111), testOnboardingBinding(111), later, later.Add(time.Minute)); err != nil {
		t.Fatalf("begin after tombstone pruning = %v", err)
	}
	if got := len(readTestOnboardingEntries(t, store)); got != 1 {
		t.Fatalf("entries after pruning = %d", got)
	}
}

func TestOnboardingStateStoreLateCancellationRollsBack(t *testing.T) {
	t.Run("late cancellation", func(t *testing.T) {
		store, _ := newTestOnboardingStateStore(t)
		now := testOnboardingTime()
		state := testOnboardingState(130)
		binding := testOnboardingBinding(130)
		if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		ctx := newLateCancelContext(2)
		if _, err := store.Claim(ctx, state, binding, testCallbackCodeDigest("code-130"), "claim-130", now); !errors.Is(err, context.Canceled) {
			t.Fatalf("late claim error = %v", err)
		}
		entry := readTestOnboardingEntries(t, store)[0]
		if entry.status != onboardingStateStatusPending {
			t.Fatalf("late cancellation advanced status to %q", entry.status)
		}
		claim, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-130"), "claim-130", now)
		if err != nil || claim.Disposition() != CallbackClaimExchangeOnce {
			t.Fatalf("claim after cancellation = %#v, %v", claim, err)
		}
	})

}

func TestOnboardingStateStoreContextsValidationAndRedaction(t *testing.T) {
	store, _ := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(137)
	binding := OnboardingFlowBinding{FlowID: "flow-secret-137", ReturnPath: "/return/path-secret-137"}
	codeHash := testCallbackCodeDigest("code-secret-137")
	claimID := "claim-secret-137"
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Begin(canceled, state, binding, now, now.Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin error = %v", err)
	}
	if entries := readTestOnboardingEntries(t, store); len(entries) != 0 {
		t.Fatalf("canceled begin stored %#v", entries)
	}
	if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(canceled, state, binding, codeHash, claimID, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("claim error = %v", err)
	}
	claim, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(canceled, claim, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("complete error = %v", err)
	}
	replay, err := store.Claim(context.Background(), state, binding, codeHash, claimID, now)
	if err != nil || replay.Disposition() != CallbackClaimReconcileOnly {
		t.Fatal("canceled complete changed durable state")
	}

	invalidBegin := []struct {
		state   string
		binding OnboardingFlowBinding
		now     time.Time
		expires time.Time
	}{
		{state: state + "=", binding: binding, now: now, expires: now.Add(time.Minute)},
		{state: base64.RawURLEncoding.EncodeToString(make([]byte, 31)), binding: binding, now: now, expires: now.Add(time.Minute)},
		{state: state, binding: OnboardingFlowBinding{FlowID: "bad flow", ReturnPath: binding.ReturnPath}, now: now, expires: now.Add(time.Minute)},
		{state: state, binding: OnboardingFlowBinding{FlowID: binding.FlowID, ReturnPath: "https://example.com/steal"}, now: now, expires: now.Add(time.Minute)},
		{state: state, binding: OnboardingFlowBinding{FlowID: binding.FlowID, ReturnPath: "//example.com/steal"}, now: now, expires: now.Add(time.Minute)},
		{state: state, binding: OnboardingFlowBinding{FlowID: binding.FlowID, ReturnPath: "/invalid-\xff"}, now: now, expires: now.Add(time.Minute)},
		{state: state, binding: binding, now: now.Local(), expires: now.Add(time.Minute)},
		{state: state, binding: binding, now: now, expires: now.Add(maxOnboardingStateLifetime + time.Nanosecond)},
	}
	for _, test := range invalidBegin {
		other, _ := newTestOnboardingStateStore(t)
		if err := other.Begin(context.Background(), test.state, test.binding, test.now, test.expires); !errors.Is(err, ErrInvalidOnboardingState) {
			t.Fatalf("validation error = %v", err)
		}
	}
	if _, err := store.Claim(context.Background(), state, binding, codeHash, "", now); !errors.Is(err, ErrOnboardingStateRejected) {
		t.Fatalf("empty claim ID = %v", err)
	}
	if _, err := store.Claim(context.Background(), state, binding, codeHash, strings.Repeat("x", maxOnboardingClaimIDBytes+1), now); !errors.Is(err, ErrOnboardingStateRejected) {
		t.Fatalf("long claim ID = %v", err)
	}
	if _, err := store.Claim(context.Background(), state, binding, codeHash, "claim\ncontrol", now); !errors.Is(err, ErrOnboardingStateRejected) {
		t.Fatalf("control claim ID = %v", err)
	}
	if _, err := store.Claim(context.Background(), state, binding, [sha256.Size]byte{}, "other-claim", now); !errors.Is(err, ErrOnboardingStateRejected) {
		t.Fatalf("zero code digest = %v", err)
	}
	if err := store.Quarantine(context.Background(), claim, CallbackQuarantineReason("secret-invalid-reason"), now); !errors.Is(err, ErrOnboardingStateRejected) {
		t.Fatalf("invalid quarantine reason = %v", err)
	}

	formatted := fmt.Sprintf("%#v", claim)
	assertRedacted(t, errors.New(formatted), state, claimID, "code-secret-137")
	closed, _ := newTestOnboardingStateStore(t)
	if err := closed.db.Close(); err != nil {
		t.Fatal(err)
	}
	err = closed.Complete(context.Background(), claim, now)
	if !errors.Is(err, ErrOnboardingStateStoreIO) {
		t.Fatalf("complete on a closed database = %v", err)
	}
	assertRedacted(t, err, state, binding.FlowID, binding.ReturnPath, claimID)
}

func newTestOnboardingStateStore(t *testing.T) (*OnboardingStateStore, string) {
	t.Helper()
	database, path := taskstoretest.Open(t)
	return NewOnboardingStateStore(database.DB()), path
}

func testOnboardingTime() time.Time {
	return time.Date(2026, time.August, 22, 12, 0, 0, 123456789, time.UTC)
}

func testOnboardingState(seed byte) string {
	value := make([]byte, 32)
	for i := range value {
		value[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func testOnboardingBinding(id int) OnboardingFlowBinding {
	return OnboardingFlowBinding{
		FlowID:     fmt.Sprintf("flow-%d", id),
		ReturnPath: fmt.Sprintf("/github/app/return/%d", id),
	}
}

func testCallbackCodeDigest(code string) [sha256.Size]byte {
	return sha256.Sum256([]byte(code))
}

func readTestOnboardingEntries(t *testing.T, store *OnboardingStateStore) []onboardingStateEntry {
	t.Helper()
	rows, err := store.db.Query(`SELECT ` + onboardingStateColumns + ` FROM onboarding_states ORDER BY issued_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var entries []onboardingStateEntry
	for rows.Next() {
		entry, err := scanOnboardingState(rows)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertRedacted(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error to inspect")
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposes secret %q: %v", secret, err)
		}
	}
}

type lateCancelContext struct {
	cancelAt int32
	calls    atomic.Int32
	done     chan struct{}
	once     sync.Once
}

func newLateCancelContext(cancelAt int32) *lateCancelContext {
	return &lateCancelContext{cancelAt: cancelAt, done: make(chan struct{})}
}

func (ctx *lateCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *lateCancelContext) Done() <-chan struct{}       { return ctx.done }
func (ctx *lateCancelContext) Value(any) any               { return nil }

func (ctx *lateCancelContext) Err() error {
	if ctx.calls.Add(1) >= ctx.cancelAt {
		ctx.once.Do(func() { close(ctx.done) })
		return context.Canceled
	}
	return nil
}
