package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

const testImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type memoryIntentStore struct{}

func (memoryIntentStore) BeginPause(string, string) error                { return nil }
func (memoryIntentStore) CommitPause(string, string) error               { return nil }
func (memoryIntentStore) CommitFailedStart(string, string) error         { return nil }
func (memoryIntentStore) CommitShutdown(string, string, time.Time) error { return nil }
func (memoryIntentStore) PauseStatus(string, string, time.Time) (PauseIntentStatus, error) {
	return PauseIntentNone, nil
}
func (memoryIntentStore) Clear(string) error { return nil }

type recordingIntentStore struct {
	status    PauseIntentStatus
	beginErr  error
	commitErr error
	clears    atomic.Int32
}

func TestResolveImageIDIsReadOnlyAndCanonical(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/v1.48/images/image:test/json" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		writeJSON(writer, http.StatusOK, map[string]any{"Id": testImageID})
	}))
	defer server.Close()
	docker := testDocker(t, server)
	imageID, err := docker.ResolveImageID(context.Background(), "image:test")
	if err != nil || imageID != testImageID || calls.Load() != 1 {
		t.Fatalf("image ID=%q calls=%d err=%v", imageID, calls.Load(), err)
	}
}

func TestResolveImageIDRejectsInvalidReferenceAndDaemonIdentity(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"Id": "image:test"})
	}))
	defer server.Close()
	docker := testDocker(t, server)
	if _, err := docker.ResolveImageID(context.Background(), " image:test"); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("invalid reference error = %v", err)
	}
	if _, err := docker.ResolveImageID(context.Background(), "image:test"); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("invalid daemon identity error = %v", err)
	}
}

func TestResolveBackgroundRunImageIDRequiresExactLabelsAndIsReadOnly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		mutate    func(map[string]any)
		expected  string
		wantError bool
	}{
		{name: "qualified"},
		{name: "wrong image ID", expected: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wantError: true},
		{name: "wrong source", mutate: mutateBackgroundConfig("Labels", backgroundSourceLabel, "other"), wantError: true},
		{name: "wrong revision", mutate: mutateBackgroundConfig("Labels", backgroundRevisionLabel, "other"), wantError: true},
		{name: "wrong version", mutate: mutateBackgroundConfig("Labels", backgroundVersionLabel, "other"), wantError: true},
		{name: "wrong profile", mutate: mutateBackgroundConfig("Labels", backgroundProfileLabel, "other"), wantError: true},
		{name: "missing profile", mutate: func(config map[string]any) { delete(config["Labels"].(map[string]string), backgroundProfileLabel) }, wantError: true},
		{name: "wrong user", mutate: func(config map[string]any) { config["User"] = "1001" }, wantError: true},
		{name: "unexpected entrypoint", mutate: func(config map[string]any) { config["Entrypoint"] = []string{"docker-entrypoint.sh"} }, wantError: true},
		{name: "wrong command", mutate: func(config map[string]any) { config["Cmd"] = []string{"opencode", "serve"} }, wantError: true},
		{name: "missing port", mutate: func(config map[string]any) { config["ExposedPorts"] = map[string]any{} }, wantError: true},
		{name: "extra port", mutate: func(config map[string]any) {
			config["ExposedPorts"] = map[string]any{"4096/tcp": map[string]any{}, "8080/tcp": map[string]any{}}
		}, wantError: true},
		{name: "baked password", mutate: func(config map[string]any) { config["Env"] = []string{"OPENCODE_SERVER_PASSWORD=secret"} }, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.Method != http.MethodGet || request.URL.Path != "/v1.48/images/background:test/json" {
					t.Errorf("unexpected Docker effect request = %s %s", request.Method, request.URL.Path)
				}
				config := map[string]any{
					"Labels": map[string]string{backgroundSourceLabel: BackgroundOpenCodeSource, backgroundRevisionLabel: BackgroundOpenCodeRevision, backgroundVersionLabel: BackgroundOpenCodeVersion, backgroundProfileLabel: BackgroundOpenCodeProfile},
					"User":   "1001:1001", "Cmd": []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"},
					"ExposedPorts": map[string]any{"4096/tcp": map[string]any{}}, "Env": []string{"XDG_DATA_HOME=/home/user/.local/share"},
				}
				if test.mutate != nil {
					test.mutate(config)
				}
				writeJSON(writer, http.StatusOK, map[string]any{"Id": testImageID, "Config": config})
			}))
			defer server.Close()
			expected := test.expected
			if expected == "" {
				expected = testImageID
			}
			imageID, err := testDocker(t, server).ResolveBackgroundRunImageID(context.Background(), "background:test", expected)
			if test.wantError {
				if !errors.Is(err, ErrSpecDrift) || imageID != "" {
					t.Fatalf("image=%q error=%v", imageID, err)
				}
			} else if err != nil || imageID != testImageID {
				t.Fatalf("image=%q error=%v", imageID, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("inspect calls = %d", calls.Load())
			}
		})
	}
}

func mutateBackgroundConfig(field, key, value string) func(map[string]any) {
	return func(config map[string]any) {
		config[field].(map[string]string)[key] = value
	}
}

func (s *recordingIntentStore) BeginPause(string, string) error {
	if s.beginErr != nil {
		return s.beginErr
	}
	s.status = PauseIntentPending
	return nil
}
func (s *recordingIntentStore) CommitPause(string, string) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	s.status = PauseIntentCommitted
	return nil
}
func (s *recordingIntentStore) CommitFailedStart(string, string) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	s.status = PauseIntentFailedStart
	return nil
}
func (s *recordingIntentStore) CommitShutdown(workspace, containerID string, _ time.Time) error {
	return s.CommitPause(workspace, containerID)
}
func (s *recordingIntentStore) PauseStatus(string, string, time.Time) (PauseIntentStatus, error) {
	return s.status, nil
}
func (s *recordingIntentStore) Clear(string) error {
	s.clears.Add(1)
	s.status = PauseIntentNone
	return nil
}

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
		// Both EnsureRunningObserved arms (StateAbsent->create,
		// StatePaused->resume) share this inspection gate, so one operation
		// covers the former Create and Resume refusals.
		{"ensure running", func() error { _, err := docker.EnsureRunningObserved(context.Background(), spec); return err }},
		{"pause", func() error { return docker.Pause(context.Background(), spec.Name) }},
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

func TestStatusAttestsActualImageIDForEveryOwnedState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		state  map[string]any
		intent PauseIntentStatus
		want   State
	}{
		{name: "running", state: map[string]any{"Status": "running", "Running": true}, want: StateRunning},
		{name: "paused", state: map[string]any{"Status": "paused", "Running": true, "Paused": true}, want: StatePaused},
		{name: "failed start frozen", state: map[string]any{"Status": "paused", "Running": true, "Paused": true}, intent: PauseIntentFailedStart, want: StateFailed},
		{name: "provisioning", state: map[string]any{"Status": "created"}, want: StateProvisioning},
		{name: "failed start created", state: map[string]any{"Status": "created"}, intent: PauseIntentFailedStart, want: StateFailed},
		{name: "failed start exited", state: map[string]any{"Status": "exited"}, intent: PauseIntentFailedStart, want: StateFailed},
		{name: "failed", state: map[string]any{"Status": "dead", "Dead": true}, want: StateFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/containers/demo/json") {
					http.NotFound(writer, request)
					return
				}
				writeJSON(writer, http.StatusOK, map[string]any{
					"Id": "container-id", "Image": testImageID,
					"Config": map[string]any{"Image": "registry.example/workspace:latest", "Labels": map[string]string{
						managedLabel: "true", workspaceLabel: "demo",
					}},
					"State": test.state, "NetworkSettings": map[string]any{"Ports": map[string]any{}},
				})
			}))
			defer server.Close()
			docker := testDocker(t, server)
			docker.intents = &recordingIntentStore{status: test.intent}
			observation, err := docker.Status(context.Background(), "demo")
			if err != nil {
				t.Fatal(err)
			}
			if observation.State != test.want || observation.ImageID != testImageID {
				t.Fatalf("observation state=%s image=%q, want state=%s image=%q", observation.State, observation.ImageID, test.want, testImageID)
			}
			if observation.ImageID == "registry.example/workspace:latest" {
				t.Fatal("actual image ID was confused with Config.Image")
			}
		})
	}
}

func TestStatusRejectsMissingOrMalformedActualImageID(t *testing.T) {
	t.Parallel()
	for _, imageID := range []string{"", "image:test", "sha256:abc", "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"} {
		t.Run(imageID, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writeJSON(writer, http.StatusOK, map[string]any{
					"Id": "container-id", "Image": imageID,
					"Config": map[string]any{"Image": "image:test", "Labels": map[string]string{
						managedLabel: "true", workspaceLabel: "demo",
					}},
					"State":           map[string]any{"Status": "running", "Running": true},
					"NetworkSettings": map[string]any{"Ports": map[string]any{}},
				})
			}))
			defer server.Close()
			observation, err := testDocker(t, server).Status(context.Background(), "demo")
			if !errors.Is(err, ErrSpecDrift) {
				t.Fatalf("Status error = %v, want ErrSpecDrift", err)
			}
			if observation != (Observation{}) {
				t.Fatalf("unsafe inspection returned observation %+v", observation)
			}
		})
	}
}

func TestStatusAbsentHasNoImageID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeDockerNotFound(writer, "container")
	}))
	defer server.Close()
	observation, err := testDocker(t, server).Status(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateAbsent || observation.ImageID != "" {
		t.Fatalf("absent observation = %+v", observation)
	}
}

func TestCreateRefusesForeignExistingVolumeWithoutMutation(t *testing.T) {
	t.Parallel()
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			writeDockerNotFound(writer, "container")
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/volumes/fern-demo-v2-data"):
			writeJSON(writer, http.StatusOK, map[string]any{"Name": "fern-demo-v2-data", "Labels": map[string]string{}})
		default:
			if request.Method != http.MethodGet {
				mutations.Add(1)
			}
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	docker := testDocker(t, server)
	if _, err := docker.EnsureRunningObserved(context.Background(), ownershipTestSpec()); !errors.Is(err, ErrUnmanaged) {
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
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/volumes/fern-demo-v2-data"):
			writeDockerNotFound(writer, "volume")
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/volumes/create"):
			writeJSON(writer, http.StatusCreated, map[string]any{"Name": "fern-demo-v2-data", "Labels": map[string]string{}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/containers/create"):
			containerCreates.Add(1)
			writeJSON(writer, http.StatusCreated, map[string]any{"Id": "should-not-exist"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	docker := testDocker(t, server)
	if _, err := docker.EnsureRunningObserved(context.Background(), ownershipTestSpec()); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("error = %v, want ErrUnmanaged", err)
	}
	if got := containerCreates.Load(); got != 0 {
		t.Fatalf("created %d containers using a foreign volume", got)
	}
}

func TestFailedInitialCreateRemovesOnlyNewVolume(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		volumeExists   bool
		wantVolumeDrop int32
	}{
		{name: "new volume", wantVolumeDrop: 1},
		{name: "existing volume", volumeExists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var volumeDrops atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
					writeDockerNotFound(writer, "container")
				case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/volumes/fern-demo-v2-data"):
					if test.volumeExists {
						writeJSON(writer, http.StatusOK, map[string]any{"Name": "fern-demo-v2-data", "Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}})
					} else {
						writeDockerNotFound(writer, "volume")
					}
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/volumes/create"):
					writeJSON(writer, http.StatusCreated, map[string]any{"Name": "fern-demo-v2-data", "Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}})
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/containers/create"):
					writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": "create failed"})
				case request.Method == http.MethodDelete && strings.HasSuffix(request.URL.Path, "/volumes/fern-demo-v2-data"):
					volumeDrops.Add(1)
					writer.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			if _, err := testDocker(t, server).EnsureRunningObserved(context.Background(), ownershipTestSpec()); err == nil {
				t.Fatal("EnsureRunningObserved unexpectedly succeeded")
			}
			if got := volumeDrops.Load(); got != test.wantVolumeDrop {
				t.Fatalf("volume removals = %d, want %d", got, test.wantVolumeDrop)
			}
		})
	}
}

func TestVerifyActualSpecAllowsImageEnvironment(t *testing.T) {
	t.Parallel()
	var restartPolicy atomic.Value
	restartPolicy.Store("no")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/containers/container-id/json") {
			http.NotFound(writer, request)
			return
		}
		useInit := true
		writeJSON(writer, http.StatusOK, map[string]any{
			"Id":    "container-id",
			"Image": testImageID,
			"Config": map[string]any{
				"Image":        "image:test",
				"User":         "1001:1001",
				"Env":          []string{"FERN=value", "PATH=/usr/local/bin", "IMAGE_VERSION=1"},
				"ExposedPorts": nat.PortSet{nat.Port(workspacePort): struct{}{}},
			},
			"HostConfig": map[string]any{
				"Memory":       1024,
				"NanoCpus":     workspaceNanoCPUs,
				"PidsLimit":    workspacePIDs,
				"Init":         useInit,
				"PortBindings": nat.PortMap{nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "49152"}}},
				"RestartPolicy": map[string]any{
					"Name": restartPolicy.Load().(string),
				},
				"CapDrop":     []string{"ALL"},
				"SecurityOpt": []string{"no-new-privileges"},
			},
			"Mounts": []map[string]any{
				{"Type": "bind", "Source": "/repo", "Destination": "/home/user/workspace", "RW": true},
				{"Type": "volume", "Name": "fern-demo-v2-data", "Destination": "/home/user/.local/share/opencode", "RW": true},
			},
			"State":           map[string]any{"Status": "running", "Running": true},
			"NetworkSettings": map[string]any{"Ports": map[string]any{}},
		})
	}))
	defer server.Close()

	docker := testDocker(t, server)
	spec := ownershipTestSpec()
	spec.Env = map[string]string{"FERN": "value"}
	inspect := func() container.InspectResponse {
		info, err := docker.cli.ContainerInspect(context.Background(), "container-id")
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	if err := verifyActualSpec(inspect(), spec); err != nil {
		t.Fatalf("image-provided environment caused drift: %v", err)
	}
	restartPolicy.Store("always")
	if err := verifyActualSpec(inspect(), spec); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("restart policy error = %v, want ErrSpecDrift", err)
	}
}

func TestExitedContainerWithPendingPauseIsRecoverable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/containers/demo/json") {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"Id":    "container-id",
			"Image": testImageID,
			"Config": map[string]any{"Labels": map[string]string{
				managedLabel: "true", workspaceLabel: "demo",
			}},
			"State":           map[string]any{"Status": "exited", "Running": false},
			"NetworkSettings": map[string]any{"Ports": map[string]any{}},
		})
	}))
	defer server.Close()

	docker := testDocker(t, server)
	docker.intents = &recordingIntentStore{status: PauseIntentPending}
	observation, err := docker.Status(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateProvisioning {
		t.Fatalf("pending stopped container state = %s, want provisioning", observation.State)
	}
}

func TestRunningContainerDoesNotMutatePauseIntent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/containers/demo/json") {
			http.NotFound(writer, request)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"Id":    "container-id",
			"Image": testImageID,
			"Config": map[string]any{"Labels": map[string]string{
				managedLabel: "true", workspaceLabel: "demo",
			}},
			"State":           map[string]any{"Status": "running", "Running": true},
			"NetworkSettings": map[string]any{"Ports": map[string]any{}},
		})
	}))
	defer server.Close()

	intents := &recordingIntentStore{status: PauseIntentCommitted}
	docker := testDocker(t, server)
	docker.intents = intents
	observation, err := docker.Status(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if observation.State != StateRunning || intents.status != PauseIntentCommitted || intents.clears.Load() != 0 {
		t.Fatalf("state=%s intent=%d clears=%d", observation.State, intents.status, intents.clears.Load())
	}
}

func TestPauseFailureReconciliation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		stopStatus int
		commitErr  error
	}{
		{name: "stop response", stopStatus: http.StatusInternalServerError},
		{name: "intent commit", stopStatus: http.StatusNoContent, commitErr: errors.New("disk full")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var inspections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && (strings.HasSuffix(request.URL.Path, "/containers/demo/json") || strings.HasSuffix(request.URL.Path, "/containers/container-id/json")):
					state := map[string]any{"Status": "running", "Running": true}
					if inspections.Add(1) > 1 && test.stopStatus != http.StatusNoContent {
						state = map[string]any{"Status": "exited", "Running": false}
					}
					writeJSON(writer, http.StatusOK, map[string]any{
						"Id":    "container-id",
						"Image": testImageID,
						"Config": map[string]any{"Labels": map[string]string{
							managedLabel: "true", workspaceLabel: "demo",
						}},
						"State":           state,
						"NetworkSettings": map[string]any{"Ports": map[string]any{}},
					})
				case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/stop"):
					if test.stopStatus == http.StatusNoContent {
						writer.WriteHeader(test.stopStatus)
					} else {
						writeJSON(writer, test.stopStatus, map[string]string{"message": "stop failed"})
					}
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			intents := &recordingIntentStore{commitErr: test.commitErr}
			docker := testDocker(t, server)
			docker.intents = intents
			err := docker.Pause(context.Background(), "demo")
			wantStatus := PauseIntentPending
			if test.stopStatus == http.StatusNoContent && test.commitErr == nil {
				wantStatus = PauseIntentCommitted
			}
			if err == nil && wantStatus != PauseIntentCommitted {
				t.Fatal("Pause succeeded despite unknown stop outcome")
			}
			if intents.status != wantStatus || intents.clears.Load() != 0 {
				t.Fatalf("intent status=%d clears=%d", intents.status, intents.clears.Load())
			}
		})
	}
}

func TestStopReconciliationPreservesIntentWhenInspectFails(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"message": "inspect unavailable"})
	}))
	defer server.Close()
	intents := &recordingIntentStore{status: PauseIntentPending}
	docker := testDocker(t, server)
	docker.intents = intents
	err := docker.reconcileStopError(context.Background(), "demo", "container-id", errors.New("stop failed"))
	if err == nil {
		t.Fatal("reconciliation unexpectedly succeeded")
	}
	if intents.status != PauseIntentPending || intents.clears.Load() != 0 {
		t.Fatalf("intent status=%d clears=%d", intents.status, intents.clears.Load())
	}
}

func TestPauseRetryPreservesPendingStoppedIntent(t *testing.T) {
	t.Parallel()
	intents := &recordingIntentStore{status: PauseIntentPending}
	docker := &Docker{intents: intents}
	observation := Observation{State: StateProvisioning, ContainerID: "container-id"}
	if err := docker.pauseObserved(context.Background(), "demo", observation); err == nil {
		t.Fatal("pause unexpectedly certified an unknown stopped container")
	}
	if intents.status != PauseIntentPending {
		t.Fatalf("intent status = %d, want pending", intents.status)
	}
}

func TestStopReconciliationHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	intents := &recordingIntentStore{status: PauseIntentPending}
	docker := testDocker(t, server)
	docker.intents = intents
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := docker.reconcileStopError(ctx, "demo", "container-id", errors.New("stop failed"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reconciliation error = %v, want context.Canceled", err)
	}
	if intents.status != PauseIntentPending {
		t.Fatalf("intent status = %d, want pending", intents.status)
	}
}

func TestPauseCreatedContainerRecordsCommittedIntent(t *testing.T) {
	t.Parallel()
	intents := &recordingIntentStore{}
	docker := &Docker{intents: intents}
	observation := Observation{State: StateProvisioning, DockerStatus: "created", ContainerID: "container-id"}
	if err := docker.pauseObserved(context.Background(), "demo", observation); err != nil {
		t.Fatal(err)
	}
	if intents.status != PauseIntentCommitted {
		t.Fatalf("intent status = %d, want committed", intents.status)
	}
}

func TestFailedStartRollbackCommitsDistinctIntentForStopAndFreeze(t *testing.T) {
	for _, suspend := range []SuspendKind{SuspendStop, SuspendFreeze} {
		t.Run(string(suspend), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/stop"):
					writer.WriteHeader(http.StatusNoContent)
				case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/pause"):
					writer.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			intents := &recordingIntentStore{}
			docker := testDocker(t, server)
			docker.suspend = suspend
			docker.intents = intents
			err := docker.pauseObservedWithOutcome(context.Background(), "demo", Observation{
				State: StateRunning, Running: true, ContainerID: "container-id",
			}, true)
			if err != nil {
				t.Fatal(err)
			}
			if intents.status != PauseIntentFailedStart {
				t.Fatalf("intent = %d, want failed start", intents.status)
			}
		})
	}
}

func TestFailedStartRollbackPreservesExistingFrozenPause(t *testing.T) {
	intents := &recordingIntentStore{status: PauseIntentCommitted}
	docker := &Docker{intents: intents, suspend: SuspendFreeze}
	if err := docker.pauseObservedWithOutcome(context.Background(), "demo", Observation{
		State: StatePaused, Running: true, Frozen: true, ContainerID: "container-id",
	}, true); err != nil {
		t.Fatal(err)
	}
	if intents.status != PauseIntentCommitted {
		t.Fatalf("intent = %d, want original committed pause", intents.status)
	}
}

func TestVerifyActualSpecRejectsExtraMountAndPrivileges(t *testing.T) {
	t.Parallel()
	useInit := true
	pidsLimit := workspacePIDs
	base := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{
			Resources:    container.Resources{Memory: 1024, NanoCPUs: workspaceNanoCPUs, PidsLimit: &pidsLimit},
			Init:         &useInit,
			CapDrop:      []string{"ALL"},
			SecurityOpt:  []string{"no-new-privileges"},
			PortBindings: nat.PortMap{nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "49152"}}},
		}},
		Config: &container.Config{Image: "image:test", User: "1001:1001", ExposedPorts: nat.PortSet{nat.Port(workspacePort): struct{}{}}},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/repo", Destination: "/home/user/workspace", RW: true},
			{Type: mount.TypeVolume, Name: "fern-demo-v2-data", Destination: "/home/user/.local/share/opencode", RW: true},
		},
	}
	spec := ownershipTestSpec()
	withMount := base
	withMount.Mounts = append(slices.Clone(base.Mounts), container.MountPoint{Type: mount.TypeBind, Source: "/tmp", Destination: "/extra", RW: true})
	if err := verifyActualSpec(withMount, spec); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("extra mount error = %v, want ErrSpecDrift", err)
	}
	base.HostConfig.Privileged = true
	if err := verifyActualSpec(base, spec); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("privileged error = %v, want ErrSpecDrift", err)
	}
	base.HostConfig.Privileged = false
	base.HostConfig.NanoCPUs = 0
	if err := verifyActualSpec(base, spec); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("CPU limit error = %v, want ErrSpecDrift", err)
	}
}

func TestVerifyActualSpecSeparatesWorkspaceGHMode(t *testing.T) {
	t.Parallel()
	useInit := true
	pidsLimit := workspacePIDs
	spec := ownershipTestSpec()
	spec.WorkspaceGH = true
	info := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{
			Resources:    container.Resources{Memory: 1024, NanoCPUs: workspaceNanoCPUs, PidsLimit: &pidsLimit},
			Init:         &useInit,
			CapDrop:      []string{"ALL"},
			SecurityOpt:  []string{"no-new-privileges"},
			PortBindings: nat.PortMap{nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "49152"}}},
		}},
		Config: &container.Config{
			Image: "image:test", User: "1001:1001",
			Env:          []string{githubConfigEnv + "=" + githubConfigDir},
			ExposedPorts: nat.PortSet{nat.Port(workspacePort): struct{}{}},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/repo", Destination: "/home/user/workspace", RW: true},
			{Type: mount.TypeVolume, Name: "fern-demo-v2-data", Destination: "/home/user/.local/share/opencode", RW: true},
			{Type: mount.TypeVolume, Name: "fern-demo-v1-gh-config", Destination: githubConfigDir, RW: true},
		},
	}
	if err := verifyActualSpec(info, spec); err != nil {
		t.Fatalf("valid workspace gh spec drifted: %v", err)
	}
	defaultSpec := spec
	defaultSpec.WorkspaceGH = false
	if err := verifyActualSpec(info, defaultSpec); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("default mode with gh mount error = %v, want ErrSpecDrift", err)
	}
	withoutMount := info
	withoutMount.Mounts = append([]container.MountPoint(nil), info.Mounts[:2]...)
	if err := verifyActualSpec(withoutMount, defaultSpec); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("default mode with gh environment error = %v, want ErrSpecDrift", err)
	}
	info.Mounts = info.Mounts[:2]
	if err := verifyActualSpec(info, spec); !errors.Is(err, ErrSpecDrift) {
		t.Fatalf("workspace gh mode without gh mount error = %v, want ErrSpecDrift", err)
	}
}

func TestWorkspaceGHVolumePersistsAcrossRecreation(t *testing.T) {
	t.Parallel()
	var containerPresent bool
	volumes := make(map[string]bool)
	var containerCreates, volumeCreates, volumeDrops atomic.Int32
	var creates []struct {
		Env        []string
		HostConfig struct {
			Mounts []mount.Mount
		}
	}
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		switch {
		case path == "/api/health":
			_, _ = writer.Write([]byte(`{"healthy":true}`))
		case request.Method == http.MethodGet && strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/json"):
			if !containerPresent {
				writeDockerNotFound(writer, "container")
				return
			}
			port := server.Listener.Addr().(*net.TCPAddr).Port
			writeJSON(writer, http.StatusOK, map[string]any{
				"Id": "container-id", "Image": testImageID,
				"Config": map[string]any{"Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}},
				"State":  map[string]any{"Status": "running", "Running": true},
				"NetworkSettings": map[string]any{"Ports": nat.PortMap{
					nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}},
				}},
			})
		case request.Method == http.MethodGet && strings.Contains(path, "/volumes/"):
			name := path[strings.LastIndex(path, "/")+1:]
			if !volumes[name] {
				writeDockerNotFound(writer, "volume")
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"Name": name, "Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}})
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/volumes/create"):
			var body struct{ Name string }
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode volume create: %v", err)
			}
			volumes[body.Name] = true
			volumeCreates.Add(1)
			writeJSON(writer, http.StatusCreated, map[string]any{"Name": body.Name, "Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}})
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/containers/create"):
			var body struct {
				Env        []string
				HostConfig struct {
					Mounts []mount.Mount
				}
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode container create: %v", err)
			}
			creates = append(creates, body)
			containerCreates.Add(1)
			containerPresent = true
			writeJSON(writer, http.StatusCreated, map[string]any{"Id": "container-id"})
		case request.Method == http.MethodPost && (strings.HasSuffix(path, "/start") || strings.HasSuffix(path, "/stop")):
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && strings.Contains(path, "/containers/"):
			containerPresent = false
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && strings.Contains(path, "/volumes/"):
			volumeDrops.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	})
	server.Start()
	defer server.Close()

	docker := testDocker(t, server)
	spec := ownershipTestSpec()
	spec.WorkspaceGH = true
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := docker.EnsureRunningObserved(context.Background(), spec); err != nil {
			t.Fatalf("create attempt %d: %v", attempt+1, err)
		}
		if err := docker.Destroy(context.Background(), spec.Name); err != nil {
			t.Fatalf("destroy attempt %d: %v", attempt+1, err)
		}
	}
	if containerCreates.Load() != 2 || volumeCreates.Load() != 2 || volumeDrops.Load() != 0 {
		t.Fatalf("container creates=%d volume creates=%d volume drops=%d", containerCreates.Load(), volumeCreates.Load(), volumeDrops.Load())
	}
	for index, create := range creates {
		if !slices.Contains(create.Env, githubConfigEnv+"="+githubConfigDir) {
			t.Fatalf("create %d environment = %v", index+1, create.Env)
		}
		if len(create.HostConfig.Mounts) != 3 || create.HostConfig.Mounts[2].Source != specGHVolumeName(spec) || create.HostConfig.Mounts[2].Target != githubConfigDir {
			t.Fatalf("create %d mounts = %+v", index+1, create.HostConfig.Mounts)
		}
	}
}

func TestReconcileStartupUsesOneInspectionForRunningContainer(t *testing.T) {
	t.Parallel()
	spec := ownershipTestSpec()
	fingerprint, err := specFingerprint(spec)
	if err != nil {
		t.Fatal(err)
	}
	var inspections atomic.Int32
	server := httptest.NewUnstartedServer(nil)
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			inspections.Add(1)
			useInit := true
			port := server.Listener.Addr().(*net.TCPAddr).Port
			writeJSON(writer, http.StatusOK, map[string]any{
				"Id":    "container-id",
				"Image": testImageID,
				"Config": map[string]any{
					"Image": spec.Image, "User": "1001:1001", "Env": sortedEnv(spec.Env),
					"Labels":       map[string]string{managedLabel: "true", workspaceLabel: spec.Name, specFingerprintLabel: fingerprint},
					"ExposedPorts": nat.PortSet{nat.Port(workspacePort): struct{}{}},
				},
				"HostConfig": map[string]any{
					"Memory": spec.MemoryBytes, "NanoCpus": workspaceNanoCPUs, "PidsLimit": workspacePIDs, "Init": useInit,
					"PortBindings":  nat.PortMap{nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}}},
					"RestartPolicy": map[string]any{"Name": "no"},
					"CapDrop":       []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"},
				},
				"Mounts": []map[string]any{
					{"Type": "bind", "Source": spec.RepoPath, "Destination": "/home/user/workspace", "RW": true},
					{"Type": "volume", "Name": specDataVolumeName(spec), "Destination": "/home/user/.local/share/opencode", "RW": true},
				},
				"State": map[string]any{"Status": "running", "Running": true},
				"NetworkSettings": map[string]any{"Ports": nat.PortMap{
					nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}},
				}},
			})
		case request.URL.Path == "/api/health":
			_, _ = writer.Write([]byte(`{"healthy":true}`))
		default:
			http.NotFound(writer, request)
		}
	})
	server.Start()
	defer server.Close()
	docker := testDocker(t, server)
	result, err := docker.ReconcileStartup(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Running || result.Transitioned || result.Endpoint.Port != server.Listener.Addr().(*net.TCPAddr).Port {
		t.Fatalf("startup result = %+v", result)
	}
	if result.ImageID != testImageID {
		t.Fatalf("startup image ID = %q, want %q", result.ImageID, testImageID)
	}
	if inspections.Load() != 1 {
		t.Fatalf("container inspections = %d, want 1", inspections.Load())
	}
}

func TestReconcileStartupLeavesAbsentAndPausedDormant(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status string
		absent bool
	}{
		{name: "absent", absent: true},
		{name: "paused", status: "paused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var inspections, mutations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					mutations.Add(1)
				}
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json") {
					inspections.Add(1)
					if test.absent {
						writeDockerNotFound(writer, "container")
						return
					}
					writeJSON(writer, http.StatusOK, map[string]any{
						"Id":              "container-id",
						"Image":           testImageID,
						"Config":          map[string]any{"Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}},
						"State":           map[string]any{"Status": test.status, "Running": true, "Paused": true},
						"NetworkSettings": map[string]any{"Ports": map[string]any{}},
					})
					return
				}
				http.NotFound(writer, request)
			}))
			defer server.Close()

			result, err := testDocker(t, server).ReconcileStartup(context.Background(), ownershipTestSpec())
			if err != nil {
				t.Fatal(err)
			}
			if result.Running || result.Endpoint != (Endpoint{}) || result.Transitioned {
				t.Fatalf("startup result = %+v, want dormant", result)
			}
			if inspections.Load() != 1 || mutations.Load() != 0 {
				t.Fatalf("inspections=%d mutations=%d, want 1 and 0", inspections.Load(), mutations.Load())
			}
		})
	}
}

func TestFrozenPauseRecordsIntentBeforeUnpause(t *testing.T) {
	t.Parallel()
	want := errors.New("intent unavailable")
	docker := &Docker{intents: &recordingIntentStore{beginErr: want}}
	observation := Observation{State: StatePaused, ContainerID: "container-id", Running: true, Frozen: true}
	if err := docker.pauseObserved(context.Background(), "demo", observation); !errors.Is(err, want) {
		t.Fatalf("pause error = %v, want intent failure", err)
	}
}

func TestDestroyCreatedContainerDoesNotStopIt(t *testing.T) {
	t.Parallel()
	var stops, removes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			writeJSON(writer, http.StatusOK, map[string]any{
				"Id":    "created-id",
				"Image": testImageID,
				"Config": map[string]any{"Labels": map[string]string{
					managedLabel: "true", workspaceLabel: "demo",
				}},
				"State":           map[string]any{"Status": "created", "Running": false},
				"NetworkSettings": map[string]any{"Ports": map[string]any{}},
			})
		case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/stop"):
			stops.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete && strings.Contains(request.URL.Path, "/containers/created-id"):
			removes.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	if err := testDocker(t, server).Destroy(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 0 || removes.Load() != 1 {
		t.Fatalf("stop calls = %d, remove calls = %d", stops.Load(), removes.Load())
	}
}

func TestDestroyAbsentWorkspaceClearsStaleIntent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json") {
			writeDockerNotFound(writer, "container")
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	intents := &recordingIntentStore{status: PauseIntentPending}
	docker := testDocker(t, server)
	docker.intents = intents
	if err := docker.Destroy(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if intents.status != PauseIntentNone || intents.clears.Load() != 1 {
		t.Fatalf("intent status=%d clears=%d, want none and one clear", intents.status, intents.clears.Load())
	}
}

func TestExecWorkspaceGHAttestsContainerAndFixesExecutionIdentity(t *testing.T) {
	t.Parallel()
	var options container.ExecOptions
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json"):
			writeJSON(writer, http.StatusOK, map[string]any{
				"Id": "container-id", "Image": testImageID,
				"Config":          map[string]any{"Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}},
				"State":           map[string]any{"Status": "running", "Running": true},
				"NetworkSettings": map[string]any{"Ports": map[string]any{}},
			})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/containers/container-id/exec"):
			if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
				t.Errorf("decode exec options: %v", err)
			}
			writeJSON(writer, http.StatusCreated, map[string]any{"Id": "abcdef123456"})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/exec/abcdef123456/start"):
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("server does not support hijacking")
				return
			}
			connection, buffered, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_, _ = buffered.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = buffered.Flush()
			_, _ = stdcopy.NewStdWriter(connection, stdcopy.Stdout).Write([]byte("safe output\n"))
			_, _ = stdcopy.NewStdWriter(connection, stdcopy.Stderr).Write([]byte("diagnostic\n"))
			// Half-close so the client observes clean EOF after draining the
			// frames. A full immediate close can deliver RST while the client
			// is still reading, which intermittently fails StdCopy under
			// race-detector scheduling.
			if tcp, ok := connection.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
			time.Sleep(25 * time.Millisecond)
			_ = connection.Close()
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/exec/abcdef123456/json"):
			writeJSON(writer, http.StatusOK, map[string]any{"Running": false, "ExitCode": 0})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := testDockerHijack(t, server).ExecWorkspaceGH(ctx, "demo", testImageID, "api", "user")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "safe output\n" || string(result.Stderr) != "diagnostic\n" || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if options.User != "1001:1001" || options.WorkingDir != "/home/user/workspace" || !options.AttachStdout || !options.AttachStderr {
		t.Fatalf("exec options = %+v", options)
	}
	wantSuffix := []string{"/usr/bin/timeout", "--signal=KILL", "--kill-after=1s", "9s", workspaceGHBinary, "api", "user"}
	if !slices.Equal(options.Cmd, wantSuffix) {
		t.Fatalf("command = %q, want %q", options.Cmd, wantSuffix)
	}
}

func TestExecWorkspaceGHRejectsUnattestedImageBeforeExec(t *testing.T) {
	t.Parallel()
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			mutations.Add(1)
		}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/containers/demo/json") {
			writeJSON(writer, http.StatusOK, map[string]any{
				"Id": "container-id", "Image": testImageID,
				"Config":          map[string]any{"Labels": map[string]string{managedLabel: "true", workspaceLabel: "demo"}},
				"State":           map[string]any{"Status": "running", "Running": true},
				"NetworkSettings": map[string]any{"Ports": map[string]any{}},
			})
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	other := "sha256:1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := testDocker(t, server).ExecWorkspaceGH(ctx, "demo", other, "api", "user"); !errors.Is(err, ErrCommandFailed) {
		t.Fatalf("error = %v", err)
	}
	if mutations.Load() != 0 {
		t.Fatalf("unattested container received %d mutations", mutations.Load())
	}
}

func TestBoundedCommandBufferRetainsPrefixAndReportsOverflow(t *testing.T) {
	buffer := &boundedCommandBuffer{limit: 4}
	if count, err := buffer.Write([]byte("abcdef")); err != nil || count != 6 {
		t.Fatalf("write count=%d err=%v", count, err)
	}
	if string(buffer.Bytes()) != "abcd" || !buffer.exceeded {
		t.Fatalf("buffer=%q exceeded=%v", buffer.Bytes(), buffer.exceeded)
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

func testDockerHijack(t *testing.T, server *httptest.Server) *Docker {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")),
		client.WithVersion("1.48"),
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
