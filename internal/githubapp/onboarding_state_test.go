package githubapp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOnboardingStateStoreClaimReplayRestartAndComplete(t *testing.T) {
	store, directory := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(1)
	binding := testOnboardingBinding(1)
	code := "manifest-code-secret-1"
	codeHash := testCallbackCodeDigest(code)
	claimID := "claim-secret-1"
	if err := store.Begin(context.Background(), state, binding, now, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	payload := readTestOnboardingPayload(t, directory)
	for _, secret := range []string{state, code, claimID} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("state file contains raw secret %q", secret)
		}
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Lstat(filepath.Join(directory, onboardingStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v", directoryInfo.Mode())
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v", fileInfo.Mode())
	}

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
	payload = readTestOnboardingPayload(t, directory)
	for _, secret := range []string{state, code, claimID} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("claimed state file contains raw secret %q", secret)
		}
	}

	restarted, err := NewOnboardingStateStore(directory)
	if err != nil {
		t.Fatal(err)
	}
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
	entries := readTestOnboardingEntries(t, directory)
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
	store, directory := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	state := testOnboardingState(4)
	binding := testOnboardingBinding(4)
	codeHash := testCallbackCodeDigest("code-4")
	claimID := "claim-4"
	if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewOnboardingStateStore(directory)
	if err != nil {
		t.Fatal(err)
	}

	var exchange, reconcile atomic.Int32
	errorsSeen := make(chan error, 32)
	var wait sync.WaitGroup
	for i := range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claimer := store
			if i%2 != 0 {
				claimer = restarted
			}
			claim, err := claimer.Claim(context.Background(), state, binding, codeHash, claimID, now)
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
	store, directory := newTestOnboardingStateStore(t)
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
	entry := readTestOnboardingEntries(t, directory)[0]
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
		store, directory := newTestOnboardingStateStore(t)
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
		entry := readTestOnboardingEntries(t, directory)[0]
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
	store, directory := newTestOnboardingStateStore(t)
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
	if got := len(readTestOnboardingEntries(t, directory)); got != maxOnboardingStates {
		t.Fatalf("entry count = %d", got)
	}
	if err := store.Begin(context.Background(), testOnboardingState(110), testOnboardingBinding(110), now, now.Add(time.Minute)); !errors.Is(err, ErrOnboardingStateLimit) {
		t.Fatalf("total cap = %v", err)
	}
	later := now.Add(5*time.Minute + maxOnboardingReplayWindow)
	if err := store.Begin(context.Background(), testOnboardingState(111), testOnboardingBinding(111), later, later.Add(time.Minute)); err != nil {
		t.Fatalf("begin after tombstone pruning = %v", err)
	}
	if got := len(readTestOnboardingEntries(t, directory)); got != 1 {
		t.Fatalf("entries after pruning = %d", got)
	}
}

func TestOnboardingStateStoreRejectsMalformedStatusChecksumAndDuplicates(t *testing.T) {
	now := testOnboardingTime()
	pending := onboardingStateEntry{
		status:     onboardingStateStatusPending,
		stateHash:  mustTestOnboardingHash(t, testOnboardingState(120)),
		flowID:     testOnboardingBinding(120).FlowID,
		returnPath: testOnboardingBinding(120).ReturnPath,
		issuedAt:   now,
		expiresAt:  now.Add(time.Minute),
	}
	claimed := pending
	claimed.status = onboardingStateStatusClaimed
	claimed.stateHash = mustTestOnboardingHash(t, testOnboardingState(121))
	claimed.flowID = testOnboardingBinding(121).FlowID
	claimed.returnPath = testOnboardingBinding(121).ReturnPath
	claimed.codeHash = testCallbackCodeDigest("malformed-code")
	claimed.claimHash = testCallbackCodeDigest("malformed-claim")
	claimed.claimedAt = now
	validPayload := mustEncodeTestEntries(t, pending)
	validClaimed := storeOnboardingStateEntry(claimed)

	missingCode := validClaimed
	missingCode.CodeHash = ""
	pendingWithClaim := storeOnboardingStateEntry(pending)
	pendingWithClaim.CodeHash = hexDigest(testCallbackCodeDigest("extra"))
	badClosed := claimed
	badClosed.status = onboardingStateStatusCompleted
	badClosed.closedAt = now.Add(2 * time.Minute)
	badClosed.retainUntil = pending.expiresAt.Add(maxOnboardingReplayWindow)
	duplicateState := pending
	duplicateState.flowID = testOnboardingBinding(122).FlowID
	duplicateState.returnPath = testOnboardingBinding(122).ReturnPath
	duplicateCode := claimed
	duplicateCode.stateHash = mustTestOnboardingHash(t, testOnboardingState(123))
	duplicateCode.flowID = testOnboardingBinding(123).FlowID
	duplicateCode.returnPath = testOnboardingBinding(123).ReturnPath
	duplicateCode.claimHash = testCallbackCodeDigest("another-claim")

	secret := "malformed-file-secret"
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "malformed", payload: []byte(`{"version":` + secret)},
		{name: "oversized", payload: []byte(strings.Repeat(secret, maxOnboardingStateFileBytes/len(secret)+2))},
		{name: "unknown version", payload: []byte(strings.Replace(string(validPayload), `"version":2`, `"version":3`, 1))},
		{name: "unknown top field", payload: []byte(strings.Replace(string(validPayload), `,"checksum"`, `,"unknown":"`+secret+`","checksum"`, 1))},
		{name: "duplicate top field", payload: []byte(strings.Replace(string(validPayload), `"version":2`, `"version":2,"version":2`, 1))},
		{name: "duplicate entry field", payload: []byte(strings.Replace(string(validPayload), `"flow_id":`, `"flow_id":"duplicate","flow_id":`, 1))},
		{name: "checksum mismatch", payload: []byte(strings.Replace(string(validPayload), `"return_path":"/github/app/return/120"`, `"return_path":"/github/app/return/124"`, 1))},
		{name: "noncanonical whitespace", payload: append([]byte(" "), validPayload...)},
		{name: "unknown status", payload: mustMarshalTestStored(t, func() []storedOnboardingStateEntry {
			e := storeOnboardingStateEntry(pending)
			e.Status = "unknown"
			return []storedOnboardingStateEntry{e}
		}())},
		{name: "claimed missing code", payload: mustMarshalTestStored(t, []storedOnboardingStateEntry{missingCode})},
		{name: "pending extra field", payload: mustMarshalTestStored(t, []storedOnboardingStateEntry{pendingWithClaim})},
		{name: "invalid closed timestamp", payload: mustMarshalTestStored(t, []storedOnboardingStateEntry{storeOnboardingStateEntry(badClosed)})},
		{name: "duplicate state digest", payload: mustMarshalTestStored(t, sortedTestStoredEntries(storeOnboardingStateEntry(pending), storeOnboardingStateEntry(duplicateState)))},
		{name: "duplicate code digest", payload: mustMarshalTestStored(t, sortedTestStoredEntries(storeOnboardingStateEntry(claimed), storeOnboardingStateEntry(duplicateCode)))},
		{name: "unsorted records", payload: mustMarshalTestStored(t, reverseTestStoredEntries(storeOnboardingStateEntry(claimed), storeOnboardingStateEntry(pending)))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, directory := newTestOnboardingStateStore(t)
			if err := os.WriteFile(filepath.Join(directory, onboardingStateFileName), test.payload, 0o600); err != nil {
				t.Fatal(err)
			}
			err := store.Begin(context.Background(), testOnboardingState(125), testOnboardingBinding(125), now, now.Add(time.Minute))
			if !errors.Is(err, ErrOnboardingStateStoreInvalid) {
				t.Fatalf("error = %v", err)
			}
			assertRedacted(t, err, secret, string(test.payload))
		})
	}
}

func TestOnboardingStateStoreLateCancellationAndWriteFailureRollback(t *testing.T) {
	t.Run("late cancellation", func(t *testing.T) {
		store, _ := newTestOnboardingStateStore(t)
		now := testOnboardingTime()
		state := testOnboardingState(130)
		binding := testOnboardingBinding(130)
		if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		ctx := newLateCancelContext(3)
		if _, err := store.Claim(ctx, state, binding, testCallbackCodeDigest("code-130"), "claim-130", now); !errors.Is(err, context.Canceled) {
			t.Fatalf("late claim error = %v", err)
		}
		entry := readTestOnboardingEntries(t, store.directory)[0]
		if entry.status != onboardingStateStatusPending {
			t.Fatalf("late cancellation advanced status to %q", entry.status)
		}
		claim, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-130"), "claim-130", now)
		if err != nil || claim.Disposition() != CallbackClaimExchangeOnce {
			t.Fatalf("claim after cancellation = %#v, %v", claim, err)
		}
	})

	t.Run("temporary creation failure", func(t *testing.T) {
		store, directory := newTestOnboardingStateStore(t)
		now := testOnboardingTime()
		state := testOnboardingState(131)
		binding := testOnboardingBinding(131)
		if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		remove := blockTestOnboardingTemporaries(t, directory)
		if _, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-131"), "claim-131", now); !errors.Is(err, ErrOnboardingStateStoreIO) {
			t.Fatalf("write failure = %v", err)
		}
		remove()
		claim, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-131"), "claim-131", now)
		if err != nil || claim.Disposition() != CallbackClaimExchangeOnce {
			t.Fatalf("claim after write failure = %#v, %v", claim, err)
		}

		remove = blockTestOnboardingTemporaries(t, directory)
		if err := store.Complete(context.Background(), claim, now); !errors.Is(err, ErrOnboardingStateStoreIO) {
			t.Fatalf("complete write failure = %v", err)
		}
		remove()
		replay, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-131"), "claim-131", now)
		if err != nil || replay.Disposition() != CallbackClaimReconcileOnly {
			t.Fatalf("claim after failed complete = %#v, %v", replay, err)
		}
	})
}

func TestOnboardingStateStoreAtomicReplacementAndInterruptedTemps(t *testing.T) {
	store, directory := newTestOnboardingStateStore(t)
	now := testOnboardingTime()
	if err := store.Begin(context.Background(), testOnboardingState(132), testOnboardingBinding(132), now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, onboardingStateFileName)
	oldGeneration, err := os.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer oldGeneration.Close()
	oldInfo, err := oldGeneration.Stat()
	if err != nil {
		t.Fatal(err)
	}
	interruptedPath := filepath.Join(directory, onboardingStateTempStem+"interrupted.tmp")
	const interruptedContent = "partial-sensitive-state-file"
	if err := os.WriteFile(interruptedPath, []byte(interruptedContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Begin(context.Background(), testOnboardingState(133), testOnboardingBinding(133), now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) {
		t.Fatal("state file was not atomically replaced")
	}
	content, err := os.ReadFile(interruptedPath)
	if err != nil || string(content) != interruptedContent {
		t.Fatalf("interrupted temp = %q, error = %v", content, err)
	}
	if err := requirePrivateOnboardingStateFile(oldGeneration); err != nil {
		t.Fatalf("old generation rejected: %v", err)
	}
}

func TestOnboardingStateStoreRejectsUnsafeFilesystemObjects(t *testing.T) {
	t.Run("directory permissions", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "states")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := NewOnboardingStateStore(directory); !errors.Is(err, ErrOnboardingStateStoreSecurity) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("directory symlink", func(t *testing.T) {
		parent := t.TempDir()
		realDirectory := filepath.Join(parent, "real")
		if err := os.Mkdir(realDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, "link")
		if err := os.Symlink(realDirectory, link); err != nil {
			t.Fatal(err)
		}
		if _, err := NewOnboardingStateStore(link); !errors.Is(err, ErrOnboardingStateStoreSecurity) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("state permissions", func(t *testing.T) {
		store, directory := newTestOnboardingStateStore(t)
		now := testOnboardingTime()
		state := testOnboardingState(134)
		binding := testOnboardingBinding(134)
		if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(directory, onboardingStateFileName), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-134"), "claim-134", now); !errors.Is(err, ErrOnboardingStateStoreSecurity) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("state symlink", func(t *testing.T) {
		store, directory := newTestOnboardingStateStore(t)
		external := filepath.Join(t.TempDir(), "external")
		const content = "external-sensitive-content"
		if err := os.WriteFile(external, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(directory, onboardingStateFileName)); err != nil {
			t.Fatal(err)
		}
		now := testOnboardingTime()
		err := store.Begin(context.Background(), testOnboardingState(135), testOnboardingBinding(135), now, now.Add(time.Minute))
		if !errors.Is(err, ErrOnboardingStateStoreSecurity) {
			t.Fatalf("error = %v", err)
		}
		got, err := os.ReadFile(external)
		if err != nil || string(got) != content {
			t.Fatalf("external = %q, error = %v", got, err)
		}
	})
	t.Run("state hard link", func(t *testing.T) {
		store, directory := newTestOnboardingStateStore(t)
		now := testOnboardingTime()
		state := testOnboardingState(136)
		binding := testOnboardingBinding(136)
		if err := store.Begin(context.Background(), state, binding, now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(directory, onboardingStateFileName), filepath.Join(t.TempDir(), "hard-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(context.Background(), state, binding, testCallbackCodeDigest("code-136"), "claim-136", now); !errors.Is(err, ErrOnboardingStateStoreSecurity) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestOnboardingStateStoreContextsValidationAndRedaction(t *testing.T) {
	store, directory := newTestOnboardingStateStore(t)
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
	if _, err := os.Stat(filepath.Join(directory, onboardingStateFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file error = %v", err)
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

	formatted := fmt.Sprintf("%v %#v %v %#v %v %#v", store, store, binding, binding, claim, claim)
	assertRedacted(t, errors.New(formatted), directory, state, binding.FlowID, binding.ReturnPath, claimID, "code-secret-137", hexDigest(claim.stateHash), hexDigest(claim.codeHash), hexDigest(claim.claimHash))
	store.directory = filepath.Join(directory, "private-directory-name")
	err = store.Complete(context.Background(), claim, now)
	if err == nil {
		t.Fatal("complete unexpectedly succeeded with missing directory")
	}
	assertRedacted(t, err, store.directory, state, binding.FlowID, binding.ReturnPath, claimID)
}

func newTestOnboardingStateStore(t *testing.T) (*OnboardingStateStore, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "github-app-private")
	store, err := NewOnboardingStateStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	return store, directory
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

func mustTestOnboardingHash(t *testing.T, state string) [sha256.Size]byte {
	t.Helper()
	hash, err := onboardingStateHash(state)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func readTestOnboardingPayload(t *testing.T, directory string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(directory, onboardingStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func readTestOnboardingEntries(t *testing.T, directory string) []onboardingStateEntry {
	t.Helper()
	entries, err := decodeOnboardingStateFile(readTestOnboardingPayload(t, directory))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func mustEncodeTestEntries(t *testing.T, entries ...onboardingStateEntry) []byte {
	t.Helper()
	payload, err := encodeOnboardingStateFile(entries)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mustMarshalTestStored(t *testing.T, entries []storedOnboardingStateEntry) []byte {
	t.Helper()
	payload, err := marshalOnboardingStateFile(entries)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func reverseTestStoredEntries(entries ...storedOnboardingStateEntry) []storedOnboardingStateEntry {
	if entries[0].StateHash < entries[1].StateHash {
		entries[0], entries[1] = entries[1], entries[0]
	}
	return entries
}

func sortedTestStoredEntries(entries ...storedOnboardingStateEntry) []storedOnboardingStateEntry {
	if entries[0].StateHash > entries[1].StateHash {
		entries[0], entries[1] = entries[1], entries[0]
	}
	return entries
}

func hexDigest(digest [sha256.Size]byte) string {
	return fmt.Sprintf("%x", digest)
}

func blockTestOnboardingTemporaries(t *testing.T, directory string) func() {
	t.Helper()
	firstSequence := onboardingStateTempSequence.Load() + 1
	blocked := make([]string, 100)
	for i := range blocked {
		blocked[i] = filepath.Join(directory, fmt.Sprintf("%s%d-%d.tmp", onboardingStateTempStem, os.Getpid(), firstSequence+uint64(i)))
		if err := os.WriteFile(blocked[i], []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		for _, path := range blocked {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
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
