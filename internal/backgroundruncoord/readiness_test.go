package backgroundruncoord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/store"
)

const readinessLocation = `{"directory":"/home/user/workspace","project":{"id":"prj_fixture","directory":"/home/user/workspace"}}`

type readinessServer struct {
	mu    sync.Mutex
	ready bool
	calls []string
}

func (s *readinessServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	reply := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/model":
		data := ""
		if s.ready {
			data = `{"id":"model","providerID":"provider","enabled":true}`
		}
		reply(http.StatusOK, `{"location":`+readinessLocation+`,"data":[`+data+`]}`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/agent":
		reply(http.StatusOK, `{"location":`+readinessLocation+`,"data":[{"id":"build"}]}`)
	default:
		// Prompt admission and its reconciliation stay inconclusive here; the
		// test only asserts which side of the prompt fence the run is on.
		reply(http.StatusInternalServerError, `{}`)
	}
}

func (s *readinessServer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func TestDispatchWaitsForModelAndAgentReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		name := map[bool]string{false: "unready catalog leaves run provisioning", true: "ready catalog crosses prompt fence"}[ready]
		t.Run(name, func(t *testing.T) {
			f := newScanFixture(t)
			f.admit(t)
			run, err := f.c.store.StartBackgroundRunProvisioning(context.Background(), ref(f.run(t), f.now))
			if err != nil {
				t.Fatal(err)
			}
			run, err = f.c.store.RecordBackgroundRunRuntime(context.Background(), store.RecordBackgroundRunRuntimeParams{
				BackgroundRunRef: ref(run, f.now), ContainerID: scanContainerID, ContainerStartedAt: "2026-08-31T12:00:00.123456789Z",
				RuntimeEpoch: 1, HostPort: 49152, Evidence: `{"effect":"runtime"}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			opencode := &readinessServer{ready: ready}
			server := httptest.NewServer(opencode)
			t.Cleanup(server.Close)
			client, err := backgroundopencode.New(backgroundopencode.Config{Endpoint: server.URL, Username: "opencode", Password: "pw", HTTPClient: &http.Client{Timeout: time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			operation, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err = f.c.dispatchWhenReady(operation, context.Background(), store.BackgroundRunWork{Run: run, Prompt: f.params.Prompt}, client)
			calls := opencode.snapshot()
			prompted := false
			for _, call := range calls {
				prompted = prompted || strings.HasSuffix(call, "/prompt")
			}
			r := f.run(t)
			if !ready {
				if !errors.Is(err, backgroundopencode.ErrNotReady) {
					t.Fatalf("unready dispatch error = %v", err)
				}
				if r.EffectPhase != store.BackgroundRunEffectProvisioning || r.State != store.BackgroundRunSettingUp || prompted {
					t.Fatalf("unready catalog crossed the fence: %s/%s calls=%v", r.State, r.EffectPhase, calls)
				}
				if len(calls) < 2 {
					t.Fatalf("readiness was not polled: %v", calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("ready dispatch error = %v", err)
			}
			if r.EffectPhase != store.BackgroundRunEffectPromptPending || !prompted {
				t.Fatalf("ready catalog did not dispatch: %s/%s calls=%v", r.State, r.EffectPhase, calls)
			}
			if calls[0] != "GET /api/model" || calls[1] != "GET /api/agent" {
				t.Fatalf("prompt preceded readiness: %v", calls)
			}
		})
	}
}
