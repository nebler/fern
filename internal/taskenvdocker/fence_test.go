package taskenvdocker

import (
	"context"
	"errors"
	"testing"

	runidentity "github.com/nebler/fern/internal/run"
)

func TestWriterFenceVariants(t *testing.T) {
	identity, err := runidentity.NewRuntime("container", "2026-09-05T01:02:03.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	runtime := runtimeFromIdentity(identity)
	for _, test := range []struct {
		fence              WriterFence
		kind               WriterFenceKind
		id, started, token string
	}{
		{NeverCreatedAuthority(), WriterFenceNeverCreated, "", "", ""},
		{CreatedContainerAuthority("created"), WriterFenceCreatedNeverStarted, "created", "", ""},
		{RuntimeCleanupAuthority(runtime), WriterFenceStoppedRuntime, runtime.ContainerID, runtime.StartedAt, runtime.Token},
	} {
		kind, err := validateCleanupAuthority(test.fence)
		if err != nil || kind != test.kind || test.fence.Kind() != test.kind || test.fence.ContainerID() != test.id || test.fence.StartedAt() != test.started || test.fence.Token() != test.token {
			t.Fatalf("fence %+v: kind=%v, error=%v", test.fence, kind, err)
		}
	}
	for _, fence := range []WriterFence{
		CreatedContainerAuthority(""), RuntimeCleanupAuthority(RuntimeIdentity{}),
		RuntimeCleanupAuthority(RuntimeIdentity{ContainerID: runtime.ContainerID, StartedAt: runtime.StartedAt, Token: "wrong"}),
		RuntimeCleanupAuthority(RuntimeIdentity{ContainerID: "container", StartedAt: "2026-09-05T01:02:03.100Z", Token: "wrong"}),
	} {
		if fence != (WriterFence{}) {
			t.Fatalf("invalid input produced nonzero fence: %+v", fence)
		}
	}
	for _, fence := range []WriterFence{
		{}, {kind: 255}, {kind: WriterFenceNeverCreated, containerID: "mixed"},
		{kind: WriterFenceNeverCreated, runtime: identity},
		{kind: WriterFenceCreatedNeverStarted},
		{kind: WriterFenceCreatedNeverStarted, containerID: "mixed", runtime: identity},
		{kind: WriterFenceStoppedRuntime},
		{kind: WriterFenceStoppedRuntime, containerID: "mixed", runtime: identity},
	} {
		if _, err := validateCleanupAuthority(fence); err == nil {
			t.Fatalf("accepted invalid private fixture: %+v", fence)
		}
	}
}

func TestRuntimeFenceConstructionDoesNotProveWriterStopped(t *testing.T) {
	provider, _, run := preparedProvider(t)
	created, err := provider.EnsureContainer(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	started, err := provider.StartContainer(t.Context(), run, created.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	fence := RuntimeCleanupAuthority(started.RuntimeIdentity())
	if fence.Kind() != WriterFenceStoppedRuntime {
		t.Fatal("valid runtime rejected")
	}
	if _, err := provider.RemoveContainer(t.Context(), run, fence); err == nil {
		t.Fatal("runtime fence bypassed fresh running-writer inspection")
	}
	if _, err := provider.RemoveVolume(t.Context(), run, fence); err == nil {
		t.Fatal("runtime fence bypassed fresh container absence inspection")
	}
	if _, err := provider.RemoveClone(t.Context(), run, fence); err == nil {
		t.Fatal("runtime fence bypassed fresh clone cleanup inspection")
	}
}

func TestWriterFenceKindsGateContainerCleanup(t *testing.T) {
	t.Run("created exact ID", func(t *testing.T) {
		provider, _, run := preparedProvider(t)
		created, err := provider.EnsureContainer(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.RemoveContainer(context.Background(), run, CreatedContainerAuthority(created.ContainerID)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("started rejects created authority", func(t *testing.T) {
		provider, _, run := preparedProvider(t)
		created, err := provider.EnsureContainer(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.StartContainer(context.Background(), run, created.ContainerID); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.RemoveContainer(context.Background(), run, CreatedContainerAuthority(created.ContainerID)); err == nil {
			t.Fatal("started container accepted ID-only cleanup authority")
		}
	})
	t.Run("NeverCreated lists renamed labels", func(t *testing.T) {
		provider, docker, run := preparedProvider(t)
		if _, err := provider.EnsureContainer(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		docker.info.Name = "/renamed-before-observation"
		authority := NeverCreatedAuthority()
		if _, err := provider.RemoveContainer(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("renamed NeverCreated container removal error=%v", err)
		}
		if _, err := provider.RemoveVolume(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("renamed NeverCreated volume cleanup error=%v", err)
		}
		if _, err := provider.RemoveClone(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("renamed NeverCreated clone cleanup error=%v", err)
		}
	})
}
