package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingIntentStore records journal interactions for pause-mechanism tests.
type countingIntentStore struct {
	beginN  atomic.Int32
	commitN atomic.Int32
	clearN  atomic.Int32
}

func (s *countingIntentStore) BeginPause(string, string) error {
	s.beginN.Add(1)
	return nil
}

func (s *countingIntentStore) CommitPause(string, string) error {
	s.commitN.Add(1)
	return nil
}

func (s *countingIntentStore) CommitFailedStart(string, string) error {
	s.commitN.Add(1)
	return nil
}

func (s *countingIntentStore) CommitShutdown(string, string, time.Time) error { return nil }

func (s *countingIntentStore) PauseStatus(string, string, time.Time) (PauseIntentStatus, error) {
	return PauseIntentNone, nil
}

func (s *countingIntentStore) Clear(string) error {
	s.clearN.Add(1)
	return nil
}

func (s *countingIntentStore) counts() string {
	return fmt.Sprintf("begin=%d commit=%d clear=%d", s.beginN.Load(), s.commitN.Load(), s.clearN.Load())
}

// ownedRunningInspect renders the inspect payload for an owned running container.
func ownedRunningInspect() map[string]any {
	return map[string]any{
		"Id":    "container-id",
		"Image": testImageID,
		"Config": map[string]any{
			"Image":  "registry.example/workspace:latest",
			"Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"},
		},
		"State":           map[string]any{"Status": "running", "Running": true},
		"NetworkSettings": map[string]any{"Ports": map[string]any{}},
	}
}

func newSuspendTestDocker(t *testing.T, server *httptest.Server, suspend SuspendKind, intents *countingIntentStore) *Docker {
	t.Helper()
	docker := testDocker(t, server)
	docker.suspend = suspend
	docker.intents = intents
	return docker
}

func TestPauseFreezeUsesContainerPauseAndCommitsIntent(t *testing.T) {
	t.Parallel()
	var pauseCalls, stopCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			writeJSON(writer, http.StatusOK, ownedRunningInspect())
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/containers/container-id/pause"):
			pauseCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/stop"):
			stopCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	intents := &countingIntentStore{}
	docker := newSuspendTestDocker(t, server, SuspendFreeze, intents)
	if err := docker.Pause(context.Background(), "demo"); err != nil {
		t.Fatalf("freeze pause: %v", err)
	}
	if pauseCalls.Load() != 1 || stopCalls.Load() != 0 {
		t.Fatalf("freeze mode called pause=%d stop=%d", pauseCalls.Load(), stopCalls.Load())
	}
	if intents.beginN.Load() != 1 || intents.commitN.Load() != 1 || intents.clearN.Load() != 0 {
		t.Fatalf("intent journal %s", intents.counts())
	}
}

func TestPauseStopNeverTouchesFreezer(t *testing.T) {
	t.Parallel()
	var pauseCalls, stopCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			writeJSON(writer, http.StatusOK, ownedRunningInspect())
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/containers/container-id/pause"):
			pauseCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/stop"):
			stopCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	intents := &countingIntentStore{}
	docker := newSuspendTestDocker(t, server, SuspendStop, intents)
	if err := docker.Pause(context.Background(), "demo"); err != nil {
		t.Fatalf("graceful stop pause: %v", err)
	}
	if pauseCalls.Load() != 0 || stopCalls.Load() != 1 {
		t.Fatalf("stop mode called pause=%d stop=%d", pauseCalls.Load(), stopCalls.Load())
	}
	if intents.beginN.Load() != 1 || intents.commitN.Load() != 1 {
		t.Fatalf("intent journal %s", intents.counts())
	}
}

func TestPauseFreezeAlreadyFrozenIsIdempotentCommit(t *testing.T) {
	t.Parallel()
	var pauseCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			payload := ownedRunningInspect()
			payload["State"] = map[string]any{"Status": "paused", "Running": true, "Paused": true}
			writeJSON(writer, http.StatusOK, payload)
		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/pause"):
			pauseCalls.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	intents := &countingIntentStore{}
	docker := newSuspendTestDocker(t, server, SuspendFreeze, intents)
	if err := docker.Pause(context.Background(), "demo"); err != nil {
		t.Fatalf("idempotent freeze: %v", err)
	}
	if pauseCalls.Load() != 0 {
		t.Fatal("already-frozen workspace was frozen again")
	}
	if intents.commitN.Load() != 1 {
		t.Fatalf("intent journal %s", intents.counts())
	}
}

func TestPauseFreezeFailureReconcilesAgainstObservedReality(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		state         map[string]any
		wantCleared   bool
		wantCommitted bool
	}{
		{name: "still live clears pending intent", state: map[string]any{"Status": "running", "Running": true}, wantCleared: true},
		{name: "frozen despite error commits intent", state: map[string]any{"Status": "paused", "Running": true, "Paused": true}, wantCommitted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/json") &&
					(strings.HasSuffix(request.URL.Path, "/containers/demo/json") || strings.HasSuffix(request.URL.Path, "/containers/container-id/json")) {
					payload := ownedRunningInspect()
					payload["State"] = test.state
					writeJSON(writer, http.StatusOK, payload)
					return
				}
				if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/pause") {
					http.Error(writer, `{"message":"OCI runtime pause failed: timeout"}`, http.StatusInternalServerError)
					return
				}
				http.NotFound(writer, request)
			}))
			defer server.Close()

			intents := &countingIntentStore{}
			docker := newSuspendTestDocker(t, server, SuspendFreeze, intents)
			err := docker.reconcileFreezeError(context.Background(), "demo", "container-id",
				fmt.Errorf(`OCI runtime pause failed: timeout`))
			if err == nil {
				t.Fatal("expected the freeze failure to surface")
			}
			if !strings.Contains(err.Error(), "unknown freeze outcome") {
				t.Fatalf("error = %v", err)
			}
			if test.wantCleared && intents.clearN.Load() != 1 {
				t.Fatalf("live container: %s", intents.counts())
			}
			if test.wantCommitted && intents.commitN.Load() < 1 {
				t.Fatalf("frozen container: %s", intents.counts())
			}
		})
	}
}

func TestNewDockerRejectsUnknownSuspendKind(t *testing.T) {
	t.Parallel()
	if _, err := NewDocker(nil, &countingIntentStore{}, SuspendKind("sigkill")); err == nil ||
		!strings.Contains(err.Error(), "unsupported idle suspend mechanism") {
		t.Fatalf("err = %v", err)
	}
}
