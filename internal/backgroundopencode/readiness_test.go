package backgroundopencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testReadiness = ReadinessSpec{Agent: "contract", ProviderID: "test", ModelID: "test-model", Directory: "/home/user/workspace"}

const testLocation = `{"directory":"/home/user/workspace","project":{"id":"prj_contract","directory":"/home/user/workspace"}}`

// modelJSON mirrors ModelV2.Info at the pinned OpenCode source commit.
func modelJSON(provider, model string, enabled bool) string {
	return fmt.Sprintf(`{"id":%q,"providerID":%q,"name":%q,"api":{"id":%q,"type":"aisdk","package":"@ai-sdk/openai-compatible","url":"http://provider:4100/v1"},"capabilities":{"tools":true,"input":["text"],"output":["text"]},"request":{"headers":{},"body":{}},"variants":[],"time":{"released":0},"cost":[],"status":"active","enabled":%t,"limit":{"context":8192,"output":4096}}`, model, provider, model, model, enabled)
}

// agentJSON mirrors AgentV2.Info at the pinned OpenCode source commit.
func agentJSON(id string) string {
	return fmt.Sprintf(`{"id":%q,"request":{"headers":{},"body":{}},"mode":"primary","hidden":false,"permissions":[]}`, id)
}

func catalogJSON(location string, items ...string) string {
	return fmt.Sprintf(`{"location":%s,"data":[%s]}`, location, strings.Join(items, ","))
}

type catalogServer struct {
	models, agents         atomic.Value
	modelCalls, agentCalls atomic.Int32
	badQuery               atomic.Bool
}

func newCatalogServer(models, agents string) *catalogServer {
	s := &catalogServer{}
	s.models.Store(models)
	s.agents.Store(agents)
	return s
}

func (s *catalogServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Query().Get("location[directory]") != "/home/user/workspace" || len(r.URL.Query()) != 1 {
		s.badQuery.Store(true)
	}
	switch r.URL.Path {
	case "/api/model":
		s.modelCalls.Add(1)
		writeJSON(w, http.StatusOK, s.models.Load().(string))
	case "/api/agent":
		s.agentCalls.Add(1)
		writeJSON(w, http.StatusOK, s.agents.Load().(string))
	default:
		http.NotFound(w, r)
	}
}

func TestModelReadyRequiresExactEnabledModelAndAgent(t *testing.T) {
	ready := catalogJSON(testLocation, modelJSON("other", "test-model", true), modelJSON("test", "test-model", true))
	agents := catalogJSON(testLocation, agentJSON("build"), agentJSON("contract"))
	for _, tt := range []struct {
		name           string
		models, agents string
		want           bool
		agentCalls     int32
	}{
		{"ready", ready, agents, true, 1},
		{"empty catalog", catalogJSON(testLocation), agents, false, 0},
		{"other provider only", catalogJSON(testLocation, modelJSON("other", "test-model", true)), agents, false, 0},
		{"disabled model", catalogJSON(testLocation, modelJSON("test", "test-model", false)), agents, false, 0},
		{"agent missing", ready, catalogJSON(testLocation, agentJSON("build")), false, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newCatalogServer(tt.models, tt.agents)
			client, _ := testClient(t, server)
			got, err := client.ModelReady(deadline(t), testReadiness)
			if err != nil || got != tt.want {
				t.Fatalf("ModelReady = %t, %v; want %t", got, err, tt.want)
			}
			if server.badQuery.Load() || server.modelCalls.Load() != 1 || server.agentCalls.Load() != tt.agentCalls {
				t.Fatalf("badQuery=%t model calls=%d agent calls=%d", server.badQuery.Load(), server.modelCalls.Load(), server.agentCalls.Load())
			}
		})
	}
}

func TestModelReadyRejectsProtocolViolations(t *testing.T) {
	agents := catalogJSON(testLocation, agentJSON("contract"))
	model := modelJSON("test", "test-model", true)
	for _, tt := range []struct{ name, models, agents string }{
		{"other location", catalogJSON(`{"directory":"/elsewhere","project":{"id":"prj_contract","directory":"/elsewhere"}}`, model), agents},
		{"workspace location", catalogJSON(`{"directory":"/home/user/workspace","workspaceID":"wrk_1","project":{"id":"prj_contract","directory":"/home/user/workspace"}}`, model), agents},
		{"missing location", `{"data":[` + model + `]}`, agents},
		{"unknown envelope field", `{"location":` + testLocation + `,"data":[],"extra":1}`, agents},
		{"null data", `{"location":` + testLocation + `,"data":null}`, agents},
		{"duplicate key", `{"location":` + testLocation + `,"data":[],"data":[]}`, agents},
		{"non-object entry", catalogJSON(testLocation, `"test-model"`), agents},
		{"entry without enabled", catalogJSON(testLocation, `{"id":"test-model","providerID":"test"}`), agents},
		{"entry with invalid identity", catalogJSON(testLocation, `{"id":"a b","providerID":"test","enabled":true}`), agents},
		{"invalid agent entry", catalogJSON(testLocation, model), catalogJSON(testLocation, `{"mode":"primary"}`)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := testClient(t, newCatalogServer(tt.models, tt.agents))
			ready, err := client.ModelReady(deadline(t), testReadiness)
			if ready || !errors.Is(err, ErrProtocol) {
				t.Fatalf("ModelReady = %t, %v", ready, err)
			}
			if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "test-model") {
				t.Fatalf("error leaks upstream detail: %v", err)
			}
		})
	}
}

func TestModelReadyRejectsInvalidSpecAndStatus(t *testing.T) {
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, `{"_tag":"ServiceUnavailableError","message":"unavailable"}`)
	}))
	for _, spec := range []ReadinessSpec{
		{Agent: "", ProviderID: "test", ModelID: "m", Directory: "/w"},
		{Agent: "a", ProviderID: "te:st", ModelID: "m", Directory: "/w"},
		{Agent: "a", ProviderID: "test", ModelID: "m", Directory: "relative"},
	} {
		if _, err := client.ModelReady(deadline(t), spec); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("spec %+v: %v", spec, err)
		}
	}
	if _, err := client.ModelReady(deadline(t), testReadiness); !errors.Is(err, ErrProtocol) {
		t.Fatalf("503: %v", err)
	}
	if _, err := client.ModelReady(context.Background(), testReadiness); !errors.Is(err, ErrDeadline) {
		t.Fatalf("no deadline: %v", err)
	}
}

func TestWaitReadyPollsUntilCatalogLoads(t *testing.T) {
	server := newCatalogServer(catalogJSON(testLocation), catalogJSON(testLocation))
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/model" && server.modelCalls.Load() == 2 {
			server.models.Store(catalogJSON(testLocation, modelJSON("test", "test-model", true)))
			server.agents.Store(catalogJSON(testLocation, agentJSON("contract")))
		}
		server.ServeHTTP(w, r)
	}))
	if err := client.WaitReady(deadline(t), testReadiness, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls := server.modelCalls.Load(); calls != 3 {
		t.Fatalf("model polls = %d", calls)
	}
}

func TestWaitReadyIsBoundedByDeadline(t *testing.T) {
	client, _ := testClient(t, newCatalogServer(catalogJSON(testLocation), catalogJSON(testLocation)))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.WaitReady(ctx, testReadiness, 20*time.Millisecond)
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("WaitReady overran its deadline: %v", elapsed)
	}
	if err := client.WaitReady(ctx, testReadiness, time.Millisecond); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("short interval: %v", err)
	}
}

func TestWaitReadyReturnsProtocolFailureImmediately(t *testing.T) {
	server := newCatalogServer(`{"data":[]}`, catalogJSON(testLocation))
	client, _ := testClient(t, server)
	if err := client.WaitReady(deadline(t), testReadiness, 10*time.Millisecond); !errors.Is(err, ErrProtocol) {
		t.Fatalf("WaitReady = %v", err)
	}
	if calls := server.modelCalls.Load(); calls != 1 {
		t.Fatalf("protocol failure retried: %d", calls)
	}
}

func TestCatalogBoundExceedsSessionBound(t *testing.T) {
	entries := make([]string, 0, 4000)
	for i := range 4000 {
		entries = append(entries, modelJSON("bulk", fmt.Sprintf("model-%d", i), true))
	}
	entries = append(entries, modelJSON("test", "test-model", true))
	models := catalogJSON(testLocation, entries...)
	if len(models) <= maxResponseBytes {
		t.Fatalf("fixture too small: %d", len(models))
	}
	client, _ := testClient(t, newCatalogServer(models, catalogJSON(testLocation, agentJSON("contract"))))
	if ready, err := client.ModelReady(deadline(t), testReadiness); err != nil || !ready {
		t.Fatalf("large catalog = %t, %v", ready, err)
	}
}
