package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/client"
)

type memoryIntentStore struct{}

func (memoryIntentStore) BeginPause(string, string) error       { return nil }
func (memoryIntentStore) CommitPause(string, string) error      { return nil }
func (memoryIntentStore) IsPaused(string, string) (bool, error) { return false, nil }
func (memoryIntentStore) Clear(string) error                    { return nil }

func TestLifecycleRefusesForeignContainerWithoutMutation(t *testing.T) {
	t.Parallel()
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			mutations.Add(1)
		}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json") {
			writeJSON(writer, http.StatusOK, map[string]any{
				"Id":              "foreign-container-id",
				"Config":          map[string]any{"Labels": map[string]string{}},
				"State":           map[string]any{"Status": "running", "Running": true},
				"NetworkSettings": map[string]any{"Ports": map[string]any{}},
			})
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	docker := testDocker(t, server)
	spec := ownershipTestSpec()
	operations := []struct {
		name string
		run  func() error
	}{
		{"create", func() error { _, err := docker.Create(context.Background(), spec); return err }},
		{"pause", func() error { return docker.Pause(context.Background(), spec.Name) }},
		{"resume", func() error { _, err := docker.Resume(context.Background(), spec); return err }},
		{"destroy", func() error { return docker.Destroy(context.Background(), spec.Name) }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, ErrUnmanaged) {
				t.Fatalf("error = %v, want ErrUnmanaged", err)
			}
		})
	}
	if got := mutations.Load(); got != 0 {
		t.Fatalf("foreign container received %d mutation requests", got)
	}
}

func TestCreateRefusesForeignExistingVolumeWithoutMutation(t *testing.T) {
	t.Parallel()
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			writeDockerNotFound(writer, "container")
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/volumes/fern-demo-data"):
			writeJSON(writer, http.StatusOK, map[string]any{"Name": "fern-demo-data", "Labels": map[string]string{}})
		default:
			if request.Method != http.MethodGet {
				mutations.Add(1)
			}
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	docker := testDocker(t, server)
	if _, err := docker.Create(context.Background(), ownershipTestSpec()); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("error = %v, want ErrUnmanaged", err)
	}
	if got := mutations.Load(); got != 0 {
		t.Fatalf("foreign volume caused %d mutation requests", got)
	}
}

func TestCreateVerifiesVolumeReturnedAfterCreateRace(t *testing.T) {
	t.Parallel()
	var containerCreates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			writeDockerNotFound(writer, "container")
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/volumes/fern-demo-data"):
			writeDockerNotFound(writer, "volume")
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/volumes/create"):
			writeJSON(writer, http.StatusCreated, map[string]any{"Name": "fern-demo-data", "Labels": map[string]string{}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/containers/create"):
			containerCreates.Add(1)
			writeJSON(writer, http.StatusCreated, map[string]any{"Id": "should-not-exist"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	docker := testDocker(t, server)
	if _, err := docker.Create(context.Background(), ownershipTestSpec()); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("error = %v, want ErrUnmanaged", err)
	}
	if got := containerCreates.Load(); got != 0 {
		t.Fatalf("created %d containers using a foreign volume", got)
	}
}

func testDocker(t *testing.T, server *httptest.Server) *Docker {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.WithHost(server.URL),
		client.WithVersion("1.48"),
		client.WithHTTPClient(server.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &Docker{
		cli:     cli,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		intents: memoryIntentStore{},
	}
}

func ownershipTestSpec() Spec {
	return Spec{Name: "demo", Image: "image:test", RepoPath: "/repo", MemoryBytes: 1024}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeDockerNotFound(writer http.ResponseWriter, resource string) {
	writeJSON(writer, http.StatusNotFound, map[string]string{"message": "No such " + resource})
}
