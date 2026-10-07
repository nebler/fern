package domain

import (
	"errors"
	"strings"
	"testing"
)

func validActor() ActorSnapshot {
	return ActorSnapshot{Type: ActorDevice, ID: "phone-1", DisplayName: "Noah's phone", CredentialID: "credential-v1", Authentication: "fern_device_cookie", RequestID: "req-1"}
}

func TestActorSnapshotValidationAndAuthority(t *testing.T) {
	a := validActor()
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ActorSnapshot){
		func(a *ActorSnapshot) { a.Type = "person" }, func(a *ActorSnapshot) { a.ID = "" }, func(a *ActorSnapshot) { a.ID = "bad\n" },
		func(a *ActorSnapshot) { a.DisplayName = strings.Repeat("x", MaxActorDisplayNameBytes+1) }, func(a *ActorSnapshot) { a.CredentialID = "" },
		func(a *ActorSnapshot) { a.Authentication = "" }, func(a *ActorSnapshot) { a.RequestID = "" },
	} {
		candidate := a
		mutate(&candidate)
		if !errors.Is(candidate.Validate(), ErrInvalidActor) {
			t.Errorf("invalid actor accepted: %+v", candidate)
		}
	}
	b := a
	b.DisplayName = "Renamed"
	b.RequestID = "req-2"
	if !a.SameAuthority(b) {
		t.Error("display/request changes changed authority")
	}
	b.CredentialID = "credential-v2"
	if a.SameAuthority(b) {
		t.Error("credential change retained authority")
	}
}

func TestIdempotencyKeyValidation(t *testing.T) {
	for _, v := range []string{"a", "two words", strings.Repeat("x", 128), "!~"} {
		if _, err := ParseIdempotencyKey(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"", " leading", "trailing ", "tab\tinside", strings.Repeat("x", 129), "non-ascii-\u00e9"} {
		if _, err := ParseIdempotencyKey(v); !errors.Is(err, ErrInvalidIdempotencyKey) {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestIdempotencyClassification(t *testing.T) {
	h1, h2 := RequestHash{1}, RequestHash{2}
	base := IdempotencyRequest{Scope: IdempotencyScope{WorkspaceID: WorkspaceID("wsp_" + validUUID), CommandKind: "task.submit"}, Key: "key", RequestHash: h1, Actor: validActor()}
	tests := []struct {
		name     string
		existing *IdempotencyRequest
		mutate   func(*IdempotencyRequest)
		want     IdempotencyDisposition
	}{
		{"first", nil, nil, IdempotencyFirstUse}, {"replay", &base, nil, IdempotencyReplay},
		{"different workspace", &base, func(c *IdempotencyRequest) {
			c.Scope.WorkspaceID = WorkspaceID("wsp_0198d34d-6a50-75fb-81f2-b4a14d70ec55")
		}, IdempotencyIndependent},
		{"different command", &base, func(c *IdempotencyRequest) { c.Scope.CommandKind = "task.cancel" }, IdempotencyIndependent},
		{"different key", &base, func(c *IdempotencyRequest) { c.Key = "other" }, IdempotencyIndependent},
		{"hash conflict", &base, func(c *IdempotencyRequest) { c.RequestHash = h2 }, IdempotencyConflict},
		{"owner mismatch", &base, func(c *IdempotencyRequest) { c.Actor.ID = "phone-2"; c.RequestHash = h2 }, IdempotencyOwnerMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			incoming := base
			if tt.mutate != nil {
				tt.mutate(&incoming)
			}
			if got := ClassifyIdempotency(tt.existing, incoming); got != tt.want {
				t.Fatalf("got %v; want %v", got, tt.want)
			}
		})
	}
}
