package pluginauth

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/task"
)

func TestAuthorizationPersistsDigestsAndReusesPresentedBearer(t *testing.T) {
	controlStore := testControlStore(t)
	store, err := Open(controlStore, "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	started, err := store.Start(now)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(started.DeviceCode)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != started.DeviceCode {
		t.Fatalf("device code is not canonical 32-byte base64url: %q, %v", started.DeviceCode, err)
	}
	if started.UserCode == "" || started.UserCode == started.DeviceCode || started.AuthorizationID == "" {
		t.Fatalf("start result did not use independent durable identities: %+v", started)
	}
	credential, err := store.Approve(started.AuthorizationID, started.UserCode, operatorActor(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Poll(started.DeviceCode, now.Add(5*time.Second))
	if err != nil || result.State != PollApproved || result.CredentialID != credential.ID {
		t.Fatalf("approved poll = %+v, %v", result, err)
	}

	path, err := controlStore.AuxiliaryStatePath("pluginauth")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{started.DeviceCode, started.UserCode} {
		if strings.Contains(string(persisted), secret) {
			t.Fatalf("persisted state contains protocol secret %q", secret)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, %v", info, err)
	}

	restarted, err := Open(controlStore, "demo")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := restarted.Poll(started.DeviceCode, now.Add(10*time.Second))
	if err != nil || repeated != result {
		t.Fatalf("restarted repeat poll = %+v, %v; want %+v", repeated, err, result)
	}
	authenticated, ok, err := restarted.Authenticate(started.DeviceCode, now.Add(11*time.Second))
	if err != nil || !ok || authenticated.ID != credential.ID || authenticated.ApprovedBy.ID != operatorActor().ID {
		t.Fatalf("restarted authenticate = %+v, %t, %v", authenticated, ok, err)
	}
}

func TestAuthorizationDenialExpiryRateAndStrictCodes(t *testing.T) {
	store, err := Open(testControlStore(t), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	denied, err := store.Start(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(now.Add(500 * time.Millisecond)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rapid start error = %v", err)
	}
	if err := store.Deny(denied.AuthorizationID, denied.UserCode, operatorActor(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := store.Poll(denied.DeviceCode, now.Add(5*time.Second))
	if err != nil || result.State != PollDenied {
		t.Fatalf("denied poll = %+v, %v", result, err)
	}
	if _, err := store.Poll(denied.DeviceCode, now.Add(6*time.Second)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rapid poll error = %v", err)
	}

	expiring, err := store.Start(now.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.Poll(expiring.DeviceCode, now.Add(11*time.Minute))
	if err != nil || result.State != PollExpired {
		t.Fatalf("expired poll = %+v, %v", result, err)
	}
	for _, invalid := range []string{"", denied.DeviceCode + "=", strings.ToUpper(denied.DeviceCode)} {
		if _, _, err := store.Authenticate(invalid, now); err != nil {
			t.Fatalf("invalid credential %q returned error: %v", invalid, err)
		}
	}
	denyAtExpiry, err := store.Start(now.Add(4 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Deny(denyAtExpiry.AuthorizationID, denyAtExpiry.UserCode, operatorActor(), denyAtExpiry.ExpiresAt); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("deny at expiry error = %v", err)
		}
	}
	if result, err := store.Poll(denyAtExpiry.DeviceCode, denyAtExpiry.ExpiresAt.Add(5*time.Second)); err != nil || result.State != PollExpired {
		t.Fatalf("deny-at-expiry poll = %+v, %v", result, err)
	}
}

func TestCanceledDecisionCannotActivateOrDeny(t *testing.T) {
	for _, decision := range []string{"approve", "deny"} {
		t.Run(decision, func(t *testing.T) {
			store, err := Open(testControlStore(t), "demo")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			started, err := store.Start(now)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if decision == "approve" {
				_, err = store.ApproveContext(ctx, started.AuthorizationID, started.UserCode, operatorActor(), now.Add(time.Second))
			} else {
				err = store.DenyContext(ctx, started.AuthorizationID, started.UserCode, operatorActor(), now.Add(time.Second))
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled %s error = %v", decision, err)
			}
			if credentials, err := store.Credentials(now.Add(2 * time.Second)); err != nil || len(credentials) != 0 {
				t.Fatalf("credentials after canceled %s = %+v, %v", decision, credentials, err)
			}
			if result, err := store.Poll(started.DeviceCode, now.Add(5*time.Second)); err != nil || result.State != PollPending {
				t.Fatalf("poll after canceled %s = %+v, %v", decision, result, err)
			}
		})
	}
}

func TestPollDistinguishesExpiredAndRevokedCredentials(t *testing.T) {
	store, err := Open(testControlStore(t), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := approveTestCredential(t, store, now)
	if result, err := store.Poll(expired.deviceCode, expired.credential.ExpiresAt.Add(time.Second)); err != nil || result.State != PollExpired {
		t.Fatalf("expired credential poll = %+v, %v", result, err)
	}
	revoked := approveTestCredential(t, store, now.Add(2*time.Second))
	actor := task.ActorSnapshot{Type: task.ActorOperator, ID: "local-operator", CredentialID: "control-test", Authentication: "basic", RequestID: "revoke-poll"}
	if err := store.Revoke(revoked.credential.ID, actor, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if result, err := store.Poll(revoked.deviceCode, now.Add(5*time.Second)); err != nil || result.State != PollDenied {
		t.Fatalf("revoked credential poll = %+v, %v", result, err)
	}
}

func TestDurableRevokeCancelsOnlyRegisteredCredentialRequests(t *testing.T) {
	store, err := Open(testControlStore(t), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := approveTestCredential(t, store, now)
	second := approveTestCredential(t, store, now.Add(2*time.Second))
	firstCanceled, secondCanceled := make(chan struct{}), make(chan struct{})
	firstCancel := func() { close(firstCanceled) }
	secondCancel := func() { close(secondCanceled) }
	unregisterFirst, ok := store.RegisterRequest(first.credential.ID, now.Add(4*time.Second), firstCancel)
	if !ok {
		t.Fatal("first request was not admitted")
	}
	defer unregisterFirst()
	unregisterSecond, ok := store.RegisterRequest(second.credential.ID, now.Add(4*time.Second), secondCancel)
	if !ok {
		t.Fatal("second request was not admitted")
	}
	defer unregisterSecond()
	selfActor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: first.credential.AuthorizationID, CredentialID: first.credential.ID, Authentication: "bearer", RequestID: "self-revoke"}
	if err := store.Revoke(first.credential.ID, selfActor, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel active request")
	}
	select {
	case <-secondCanceled:
		t.Fatal("revocation canceled another credential")
	default:
	}
	if _, ok, err := store.Authenticate(first.deviceCode, now.Add(6*time.Second)); err != nil || ok {
		t.Fatalf("revoked credential authenticated: %t, %v", ok, err)
	}
}

func TestConcurrentApprovalActivatesExactlyOneCredential(t *testing.T) {
	store, err := Open(testControlStore(t), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	started, err := store.Start(now)
	if err != nil {
		t.Fatal(err)
	}
	const approvals = 16
	ids := make(chan string, approvals)
	errs := make(chan error, approvals)
	var group sync.WaitGroup
	for range approvals {
		group.Add(1)
		go func() {
			defer group.Done()
			credential, err := store.Approve(started.AuthorizationID, started.UserCode, operatorActor(), now.Add(time.Second))
			if err != nil {
				errs <- err
				return
			}
			ids <- credential.ID
		}()
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent approval: %v", err)
	}
	credentialID := ""
	for id := range ids {
		if credentialID == "" {
			credentialID = id
		}
		if id != credentialID {
			t.Fatalf("approval minted multiple credentials: %q and %q", credentialID, id)
		}
	}
	credentials, err := store.Credentials(now.Add(2 * time.Second))
	if err != nil || len(credentials) != 1 || credentials[0].ID != credentialID {
		t.Fatalf("credentials after concurrent approval = %+v, %v", credentials, err)
	}
}

func TestRegisterAndRevokeRaceLeavesNoAdmittedRequestActive(t *testing.T) {
	store, err := Open(testControlStore(t), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	approved := approveTestCredential(t, store, now)
	const requests = 32
	type registration struct {
		admitted   bool
		canceled   chan struct{}
		unregister func()
	}
	registrations := make(chan registration, requests)
	revokeErr := make(chan error, 1)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			canceled := make(chan struct{})
			unregister, admitted := store.RegisterRequest(approved.credential.ID, now.Add(2*time.Second), func() { close(canceled) })
			registrations <- registration{admitted, canceled, unregister}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		actor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: approved.credential.AuthorizationID, CredentialID: approved.credential.ID, Authentication: "bearer", RequestID: "racing-revoke"}
		if err := store.Revoke(approved.credential.ID, actor, now.Add(3*time.Second)); err != nil {
			revokeErr <- err
		}
	}()
	close(start)
	group.Wait()
	select {
	case err := <-revokeErr:
		t.Fatal(err)
	default:
	}
	close(registrations)
	for registration := range registrations {
		if !registration.admitted {
			continue
		}
		select {
		case <-registration.canceled:
		case <-time.After(time.Second):
			t.Fatal("request admitted across revoke was not canceled")
		}
		registration.unregister()
	}
}

func TestOpenRejectsCorruptAndUnsafeState(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{name: "malformed", content: `{`, mode: 0o600},
		{name: "unknown", content: `{"version":1,"revision":0,"authorizations":{},"credentials":{},"extra":true}`, mode: 0o600},
		{name: "duplicate", content: `{"version":1,"version":1,"revision":0,"authorizations":{},"credentials":{}}`, mode: 0o600},
		{name: "public", content: `{"version":1,"revision":0,"authorizations":{},"credentials":{}}`, mode: 0o644},
	} {
		t.Run(test.name, func(t *testing.T) {
			controlStore := testControlStore(t)
			path, err := controlStore.AuxiliaryStatePath("pluginauth")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.content), test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(controlStore, "demo"); err == nil {
				t.Fatal("unsafe state unexpectedly loaded")
			}
		})
	}
}

func TestOpenRejectsStateCopiedFromAnotherWorkspace(t *testing.T) {
	root := t.TempDir()
	firstControl, err := control.Open(filepath.Join(root, "control"), "first")
	if err != nil {
		t.Fatal(err)
	}
	first, err := Open(firstControl, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Start(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	firstPath, _ := firstControl.AuxiliaryStatePath("pluginauth")
	data, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondControl, err := control.Open(filepath.Join(root, "control"), "second")
	if err != nil {
		t.Fatal(err)
	}
	secondPath, _ := secondControl.AuxiliaryStatePath("pluginauth")
	if err := os.WriteFile(secondPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(secondControl, "second"); err == nil || !strings.Contains(err.Error(), "state header") {
		t.Fatalf("copied workspace state error = %v", err)
	}
}

func testControlStore(t *testing.T) *control.Store {
	t.Helper()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func operatorActor() task.ActorSnapshot {
	return task.ActorSnapshot{Type: task.ActorOperator, ID: "local-operator", DisplayName: "Local operator", CredentialID: "control-test", Authentication: "basic", RequestID: "request-test"}
}

type approvedTestCredential struct {
	credential Credential
	deviceCode string
}

func approveTestCredential(t *testing.T, store *Store, now time.Time) approvedTestCredential {
	t.Helper()
	started, err := store.Start(now)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := store.Approve(started.AuthorizationID, started.UserCode, operatorActor(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return approvedTestCredential{credential: credential, deviceCode: started.DeviceCode}
}

func TestRequestAuthorizationHasOnlyFixedScopes(t *testing.T) {
	credential := Credential{ID: "credential"}
	ctx := WithRequestAuthorization(context.Background(), credential)
	authorization, ok := RequestAuthorizationFromContext(ctx)
	if !ok || authorization.Credential.ID != credential.ID {
		t.Fatal("authorization context missing")
	}
	for _, scope := range Scopes() {
		if !authorization.HasScope(scope) {
			t.Fatalf("fixed scope %q missing", scope)
		}
	}
	if authorization.HasScope("control:admin") {
		t.Fatal("dynamic/admin scope was accepted")
	}
}
