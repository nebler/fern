package runapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/run"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
)

type stubVerifier struct {
	calls int
	err   error
}

func (v *stubVerifier) Verify(context.Context, task.GitOID) error { v.calls++; return v.err }

type commandStore struct {
	Store
	receipt                taskstore.Receipt
	current                taskstore.BackgroundRun
	found, race, committed bool
	err                    error
	admit                  taskstore.AdmitBackgroundRunParams
	seal                   taskstore.SealBackgroundRunParams
}

func (s *commandStore) FindReceiptByIdempotency(context.Context, task.WorkspaceID, string, task.IdempotencyKey) (taskstore.Receipt, bool, error) {
	return s.receipt, s.found, nil
}
func (s *commandStore) GetBackgroundRun(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.BackgroundRun, error) {
	return s.current, nil
}
func receiptFor(claim task.IdempotencyClaim, id task.TaskID, receiptID task.ReceiptID) taskstore.Receipt {
	return taskstore.Receipt{ID: receiptID, WorkspaceID: claim.Scope.WorkspaceID, CommandKind: claim.Scope.CommandKind,
		IdempotencyKey: claim.Key, RequestHash: claim.RequestHash, Actor: claim.Actor, TargetID: id}
}
func (s *commandStore) AdmitBackgroundRun(_ context.Context, p taskstore.AdmitBackgroundRunParams) (taskstore.Admission, error) {
	s.admit = p
	if s.err != nil {
		return taskstore.Admission{}, s.err
	}
	s.committed = true
	s.current.TaskID = p.TaskID
	s.receipt = receiptFor(p.Claim, p.TaskID, p.ReceiptID)
	return taskstore.Admission{Task: taskstore.Task{ID: p.TaskID}, Replayed: s.race}, nil
}
func (s *commandStore) StopBackgroundRun(_ context.Context, p taskstore.StopBackgroundRunParams) (taskstore.BackgroundRunStop, error) {
	if s.err != nil {
		return taskstore.BackgroundRunStop{}, s.err
	}
	s.committed = true
	s.current.TaskID, s.current.StopReceiptID = p.TaskID, p.ReceiptID
	s.current.State = taskstore.BackgroundRunCanceling
	if s.race {
		s.current.State = taskstore.BackgroundRunCleanupRequired // advanced since the original commit
	}
	s.receipt = receiptFor(p.Claim, p.TaskID, p.ReceiptID)
	s.receipt.ResponseProjection = json.RawMessage(`{"run_id":"` + string(p.TaskID) + `","state":"canceling"}`)
	return taskstore.BackgroundRunStop{Run: s.current, Receipt: s.receipt, Replayed: s.race}, nil
}
func (s *commandStore) GetBackgroundRunOwners(context.Context, task.WorkspaceID, task.TaskID, task.ActorSnapshot) (taskstore.Task, taskstore.Attempt, error) {
	return taskstore.Task{Revision: 7}, taskstore.Attempt{Revision: 9}, nil
}
func (s *commandStore) SealBackgroundRun(_ context.Context, p taskstore.SealBackgroundRunParams) (taskstore.BackgroundRunSealAdmission, error) {
	s.seal = p
	if s.err != nil {
		return taskstore.BackgroundRunSealAdmission{}, s.err
	}
	s.committed = true
	return taskstore.BackgroundRunSealAdmission{Run: s.current, Replayed: s.race}, nil
}

func setupService(t *testing.T) (*service, *commandStore, *stubVerifier, task.ActorSnapshot, createIntent, *int) {
	t.Helper()
	store, base := &commandStore{}, &stubVerifier{}
	wakes := new(int)
	s := &service{config: Config{WorkspaceID: testWorkspace, RepositoryRemote: "https://github.com/owner/repository", Store: store, Generator: task.NewSecureGenerator(), BaseVerifier: base,
		Now: func() time.Time { return time.Unix(1750000000, 123456789) }, AttemptTimeout: time.Hour,
		AvailableProfile: taskstore.BackgroundRunSourceProfile, SealPolicyVersion: "seal.v1",
		Wake: func() {
			if !store.committed {
				t.Error("wake before commit")
			}
			*wakes++
		},
	}}
	actor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: "pc_owner", DisplayName: "Plugin", CredentialID: "pc_owner", Authentication: "fern_plugin_bearer", RequestID: "request"}
	input := createIntent{Repository: "https://github.com/owner/repository", BaseOID: "0123456789abcdef0123456789abcdef01234567", Instruction: "Work\n\t<exact bytes> ", Profile: taskstore.BackgroundRunSourceProfile}
	return s, store, base, actor, input, wakes
}

func TestCreateCommitReplayAndIdentity(t *testing.T) {
	s, store, base, actor, input, wakes := setupService(t)
	ctx := context.Background()
	accepted, err := s.Create(ctx, actor, "create", input)
	if err != nil || !accepted.Committed || accepted.Replayed || *wakes != 1 {
		t.Fatalf("create=%+v err=%v wakes=%d", accepted, err, *wakes)
	}
	// Literal v1 encoding protects field order, null branch, escaping, and whitespace.
	wantHash := sha256.Sum256([]byte(taskstore.CreateBackgroundRunCommand + "\n" + `{"repository":"https://github.com/owner/repository","base_oid":"0123456789abcdef0123456789abcdef01234567","branch":null,"instruction":"Work\n\t\u003cexact bytes\u003e ","profile":"` + input.Profile + `"}`))
	if store.admit.Claim.RequestHash != task.RequestHash(wantHash) || store.admit.Prompt != input.Instruction || store.admit.BackgroundRun.InstructionSHA256 != sha256.Sum256([]byte(input.Instruction)) {
		t.Fatal("create bytes changed")
	}
	resources, _ := run.NewResources(accepted.RunID, 1)
	if store.admit.BackgroundRun.CloneIdentity != resources.Clone() || store.admit.BackgroundRun.EndpointIdentity != resources.Endpoint() {
		t.Fatal("resource derivation changed")
	}
	store.found = true
	base.err = errors.New("base gone")
	s.config.AvailableProfile = ""
	s.config.Now = func() time.Time { panic("replay read clock") }
	replayed, err := s.Create(ctx, actor, "create", input)
	if err != nil || !replayed.Replayed || replayed.RunID != accepted.RunID || base.calls != 1 || *wakes != 1 {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	input.Instruction += "changed"
	if _, err := s.Create(ctx, actor, "create", input); !errors.Is(err, errReplayConflict) {
		t.Fatalf("changed hash=%v", err)
	}
	actor.ID, actor.CredentialID = "pc_other", "pc_other"
	if _, err := s.Create(ctx, actor, "create", input); !errors.Is(err, taskstore.ErrNotFound) {
		t.Fatalf("owner mismatch=%v", err)
	}
	if *wakes != 1 {
		t.Fatal("replay/conflict woke")
	}
}

func TestCreateEnforcesCommandPolicyWithoutHTTP(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*createIntent, *task.ActorSnapshot)
		want   error
	}{
		{"foreign repository", func(input *createIntent, _ *task.ActorSnapshot) {
			input.Repository = "https://github.com/other/repository"
		}, errInvalidCreate},
		{"unsupported profile", func(input *createIntent, _ *task.ActorSnapshot) { input.Profile = "unknown" }, errInvalidCreate},
		{"empty instruction", func(input *createIntent, _ *task.ActorSnapshot) { input.Instruction = " \t\n" }, errInvalidCreate},
		{"control instruction", func(input *createIntent, _ *task.ActorSnapshot) { input.Instruction = "work\x00" }, errInvalidCreate},
		{"empty branch", func(input *createIntent, _ *task.ActorSnapshot) { branch := ""; input.Branch = &branch }, errInvalidCreate},
		{"symbolic base", func(input *createIntent, _ *task.ActorSnapshot) { input.BaseOID = "HEAD" }, errInvalidBase},
		{"non-plugin actor", func(_ *createIntent, actor *task.ActorSnapshot) { actor.Type = task.ActorOperator }, task.ErrInvalidActor},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, store, base, actor, input, wakes := setupService(t)
			tt.mutate(&input, &actor)
			if _, err := s.Create(context.Background(), actor, "create", input); !errors.Is(err, tt.want) {
				t.Fatalf("Create error = %v, want %v", err, tt.want)
			}
			if base.calls != 0 || store.committed || *wakes != 0 {
				t.Fatal("rejected command performed admission effects")
			}
		})
	}
}

func TestCommitRaceReplayAndFailureDoNotWake(t *testing.T) {
	for _, operation := range []string{"create", "stop", "seal"} {
		for _, mode := range []string{"fresh", "race", "failure"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				s, store, _, actor, input, wakes := setupService(t)
				ids, _ := s.config.Generator.GenerateAdmissionIDs()
				store.current = taskstore.BackgroundRun{TaskID: ids.TaskID, AttemptID: ids.AttemptID, Generation: 1, Revision: 5}
				store.race = mode == "race"
				if mode == "failure" {
					store.err = errors.New("commit failed")
				}
				var err error
				switch operation {
				case "create":
					_, err = s.Create(context.Background(), actor, "key", input)
				case "stop":
					var accepted stopAcceptance
					accepted, err = s.Stop(context.Background(), actor, "key", ids.TaskID)
					if mode == "race" && (accepted.State != run.Canceling || !accepted.Replayed) {
						t.Fatalf("stop must project receipt: %+v", accepted)
					}
				case "seal":
					_, err = s.Seal(context.Background(), actor, "key", ids.TaskID)
					if store.seal.ExpectedRunRevision != 5 || store.seal.ExpectedTaskRevision != 7 || store.seal.ExpectedAttemptRevision != 9 || store.seal.PolicyVersion != "seal.v1" || store.seal.AcceptedAt.Nanosecond() != 123000000 {
						t.Fatalf("seal assembly=%+v", store.seal)
					}
				}
				if (err != nil) != (mode == "failure") {
					t.Fatalf("error=%v", err)
				}
				want := 0
				if mode == "fresh" {
					want = 1
				}
				if *wakes != want {
					t.Fatalf("wakes=%d want=%d", *wakes, want)
				}
			})
		}
	}
}

func TestStopReplayValidatesOriginalReceipt(t *testing.T) {
	s, store, _, actor, _, wakes := setupService(t)
	ids, _ := s.config.Generator.GenerateAdmissionIDs()
	store.race = true
	if _, err := s.Stop(context.Background(), actor, "stop", ids.TaskID); err != nil {
		t.Fatal(err)
	}
	store.found = true
	s.config.Now = func() time.Time { panic("replay read clock") }
	accepted, err := s.Stop(context.Background(), actor, "stop", ids.TaskID)
	if err != nil || accepted.State != run.Canceling || !accepted.Replayed {
		t.Fatalf("replay=%+v err=%v", accepted, err)
	}
	store.receipt.ResponseProjection = json.RawMessage(`{"run_id":"` + string(ids.TaskID) + `","state":"result_ready"}`)
	if _, err := s.Stop(context.Background(), actor, "stop", ids.TaskID); !errors.Is(err, taskstore.ErrCorruptStore) {
		t.Fatalf("invalid receipt=%v", err)
	}
	store.current.StopReceiptID = ""
	if _, err := s.Stop(context.Background(), actor, "stop", ids.TaskID); !errors.Is(err, taskstore.ErrCorruptStore) {
		t.Fatalf("receipt mismatch=%v", err)
	}
	if *wakes != 0 {
		t.Fatal("replay woke")
	}
}

func TestCommandBoundaryRejectsInvalidIdentities(t *testing.T) {
	s, _, _, actor, input, wakes := setupService(t)
	if _, err := s.Create(context.Background(), task.ActorSnapshot{}, "key", input); err == nil {
		t.Fatal("invalid actor accepted")
	}
	if _, err := s.Create(context.Background(), actor, "", input); err == nil {
		t.Fatal("invalid key accepted")
	}
	input.BaseOID = "bad"
	if _, err := s.Create(context.Background(), actor, "key", input); err == nil {
		t.Fatal("invalid base accepted")
	}
	if _, err := s.Stop(context.Background(), actor, "key", "bad"); err == nil {
		t.Fatal("invalid stop ID accepted")
	}
	if _, err := s.Seal(context.Background(), actor, "key", "bad"); err == nil {
		t.Fatal("invalid seal ID accepted")
	}
	if *wakes != 0 {
		t.Fatal("invalid command woke")
	}
}
