package task

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
	h1, _ := ParseRequestHash(strings.Repeat("11", 32))
	h2, _ := ParseRequestHash(strings.Repeat("22", 32))
	base := IdempotencyClaim{Scope: IdempotencyScope{WorkspaceID: WorkspaceID("wsp_" + validUUID), CommandKind: "task.submit"}, Key: "key", RequestHash: h1, Actor: validActor()}
	tests := []struct {
		name     string
		existing *IdempotencyClaim
		mutate   func(*IdempotencyClaim)
		want     IdempotencyDisposition
	}{
		{"first", nil, nil, IdempotencyFirstUse}, {"replay", &base, nil, IdempotencyReplay},
		{"different workspace", &base, func(c *IdempotencyClaim) {
			c.Scope.WorkspaceID = WorkspaceID("wsp_0198d34d-6a50-75fb-81f2-b4a14d70ec55")
		}, IdempotencyIndependent},
		{"different command", &base, func(c *IdempotencyClaim) { c.Scope.CommandKind = "task.cancel" }, IdempotencyIndependent},
		{"different key", &base, func(c *IdempotencyClaim) { c.Key = "other" }, IdempotencyIndependent},
		{"hash conflict", &base, func(c *IdempotencyClaim) { c.RequestHash = h2 }, IdempotencyConflict},
		{"owner mismatch", &base, func(c *IdempotencyClaim) { c.Actor.ID = "phone-2"; c.RequestHash = h2 }, IdempotencyOwnerMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			incoming := base
			if tt.mutate != nil {
				tt.mutate(&incoming)
			}
			got, err := ClassifyIdempotency(tt.existing, incoming)
			if err != nil || got != tt.want {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestImmutableTuples(t *testing.T) {
	baseSHA := GitOID(strings.Repeat("1", 40))
	resultSHA := GitOID(strings.Repeat("2", 40))
	repository := RepositoryTuple{RepositoryID: 42, BaseSHA: baseSHA}
	changed := ResultTuple{RepositoryTuple: repository, ResultCommit: resultSHA, Outcome: ResultChanged, ManifestEntries: 1, WorktreeClean: true}
	if err := changed.ValidateAgainst(repository); err != nil {
		t.Fatal(err)
	}
	noChanges := ResultTuple{RepositoryTuple: repository, ResultCommit: baseSHA, Outcome: ResultNoChanges, WorktreeClean: true}
	if err := noChanges.ValidateAgainst(repository); err != nil {
		t.Fatal(err)
	}
	invalidResults := []ResultTuple{changed, changed, changed, changed, noChanges}
	invalidResults[0].RepositoryID++
	invalidResults[1].ResultCommit = baseSHA
	invalidResults[2].ManifestEntries = 0
	invalidResults[3].WorktreeClean = false
	invalidResults[4].ManifestEntries = 1
	for _, r := range invalidResults {
		if !errors.Is(r.ValidateAgainst(repository), ErrInvalidTuple) {
			t.Errorf("invalid result accepted: %+v", r)
		}
	}
	overflowRepository := RepositoryTuple{RepositoryID: RepositoryID(maxSQLiteInteger + 1), BaseSHA: baseSHA}
	if !errors.Is(overflowRepository.Validate(), ErrInvalidTuple) {
		t.Error("repository identity exceeding SQLite INTEGER was accepted")
	}

	op := PublicationOperationID("op_" + validUUID)
	publication := PublicationTuple{OperationID: op, InstallationID: 9, RepositoryID: 42, RepositoryFullName: "owner/repository", WorkspaceName: "demo", BaseRef: "main", BaseSHA: baseSHA, ResultCommit: resultSHA, Branch: PublicationBranch("demo", op)}
	verification := VerificationTuple{State: VerificationSucceeded, VerifiedCommit: resultSHA}
	if err := publication.ValidateAgainst(42, repository, changed, verification); err != nil {
		t.Fatal(err)
	}
	bad := publication
	bad.Branch = "fern/demo/wrong"
	if !errors.Is(bad.ValidateAgainst(42, repository, changed, verification), ErrInvalidTuple) {
		t.Error("wrong branch accepted")
	}
	bad = publication
	bad.RepositoryID++
	if !errors.Is(bad.ValidateAgainst(42, repository, changed, verification), ErrInvalidTuple) {
		t.Error("wrong repository accepted")
	}
	failedVerification := verification
	failedVerification.State = VerificationFailed
	if !errors.Is(publication.ValidateAgainst(42, repository, changed, failedVerification), ErrInvalidTuple) {
		t.Error("failed verification accepted")
	}
	bad = publication
	bad.ExpectedRemoteOldSHA = "invalid"
	if !errors.Is(bad.ValidateAgainst(42, repository, changed, verification), ErrInvalidTuple) {
		t.Error("invalid expected remote old SHA accepted")
	}
	bad = publication
	bad.InstallationID = InstallationID(maxSQLiteInteger + 1)
	if !errors.Is(bad.ValidateAgainst(42, repository, changed, verification), ErrInvalidTuple) {
		t.Error("installation identity exceeding SQLite INTEGER was accepted")
	}
	for _, fullName := range []string{"", "owner", "owner/repository/extra", "/repository", "owner/", ".owner/repository", "owner_/repository", "owner-/repository", "owner/repository.git", "owner/repo space", strings.Repeat("o", 40) + "/repository", "owner/" + strings.Repeat("r", 101)} {
		bad = publication
		bad.RepositoryFullName = fullName
		if !errors.Is(bad.ValidateAgainst(42, repository, changed, verification), ErrInvalidTuple) {
			t.Errorf("invalid repository full name %q accepted", fullName)
		}
	}

	observation := PublicationObservation{RemoteSHA: resultSHA, PullRequest: PullRequestObservation{
		RepositoryID: 42, RepositoryFullName: "owner/repository", Number: 17, URL: "https://github.com/owner/repository/pull/17", State: "open", Draft: true,
		BaseRepositoryID: 42, BaseRepositoryFullName: "owner/repository", BaseRef: "main", BaseSHA: baseSHA,
		HeadRepositoryID: 42, HeadRepositoryFullName: "owner/repository", HeadRepositoryOwner: "owner", HeadRepositoryName: "repository", HeadRef: publication.Branch, HeadSHA: resultSHA,
	}}
	if err := observation.ValidateAgainst(publication); err != nil {
		t.Fatal(err)
	}
	observation.PullRequest.Draft = false
	if !errors.Is(observation.ValidateAgainst(publication), ErrInvalidTuple) {
		t.Error("non-draft PR accepted")
	}

	validObservation := PublicationObservation{RemoteSHA: resultSHA, PullRequest: PullRequestObservation{
		RepositoryID: 42, RepositoryFullName: "owner/repository", Number: 17, URL: "https://github.com/owner/repository/pull/17", State: "open", Draft: true,
		BaseRepositoryID: 42, BaseRepositoryFullName: "owner/repository", BaseRef: "main", BaseSHA: baseSHA,
		HeadRepositoryID: 42, HeadRepositoryFullName: "owner/repository", HeadRepositoryOwner: "owner", HeadRepositoryName: "repository", HeadRef: publication.Branch, HeadSHA: resultSHA,
	}}
	invalidObservations := []PublicationObservation{
		validObservation, validObservation, validObservation, validObservation, validObservation, validObservation, validObservation, validObservation,
		validObservation, validObservation, validObservation, validObservation, validObservation, validObservation, validObservation, validObservation,
	}
	invalidObservations[0].RemoteSHA = baseSHA
	invalidObservations[1].PullRequest.RepositoryID++
	invalidObservations[2].PullRequest.RepositoryFullName = "other/repository"
	invalidObservations[3].PullRequest.Number = 0
	invalidObservations[4].PullRequest.URL = "https://github.com/other/repository/pull/17"
	invalidObservations[5].PullRequest.State = "closed"
	invalidObservations[6].PullRequest.BaseRepositoryID++
	invalidObservations[7].PullRequest.BaseRepositoryFullName = "other/repository"
	invalidObservations[8].PullRequest.BaseRef = "other"
	invalidObservations[9].PullRequest.BaseSHA = resultSHA
	invalidObservations[10].PullRequest.HeadRepositoryID++
	invalidObservations[11].PullRequest.HeadRepositoryFullName = "other/repository"
	invalidObservations[12].PullRequest.HeadRepositoryOwner = "other"
	invalidObservations[13].PullRequest.HeadRepositoryName = "other"
	invalidObservations[14].PullRequest.HeadRef = "other"
	invalidObservations[15].PullRequest.HeadSHA = baseSHA
	for index, invalid := range invalidObservations {
		if !errors.Is(invalid.ValidateAgainst(publication), ErrInvalidTuple) {
			t.Errorf("invalid publication observation %d accepted: %+v", index, invalid)
		}
	}
}
