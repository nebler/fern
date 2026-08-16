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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type memoryIntentStore struct{}

func (memoryIntentStore) BeginPause(string, string) error  { return nil }
func (memoryIntentStore) CommitPause(string, string) error { return nil }
func (memoryIntentStore) PauseStatus(string, string) (PauseIntentStatus, error) {
	return PauseIntentNone, nil
}
func (memoryIntentStore) Clear(string) error { return nil }

type recordingIntentStore struct {
	status    PauseIntentStatus
	beginErr  error
	commitErr error
	clears    atomic.Int32
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
func (s *recordingIntentStore) PauseStatus(string, string) (PauseIntentStatus, error) {
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
	if _, err := docker.Create(context.Background(), ownershipTestSpec()); !errors.Is(err, ErrUnmanaged) {
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

			if _, err := testDocker(t, server).Create(context.Background(), ownershipTestSpec()); err == nil {
				t.Fatal("Create unexpectedly succeeded")
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
			"Id": "container-id",
			"Config": map[string]any{
				"Image":        "image:test",
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
	if err := docker.verifyActualSpec(context.Background(), "container-id", spec); err != nil {
		t.Fatalf("image-provided environment caused drift: %v", err)
	}
	restartPolicy.Store("always")
	if err := docker.verifyActualSpec(context.Background(), "container-id", spec); !errors.Is(err, ErrSpecDrift) {
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
			"Id": "container-id",
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
			"Id": "container-id",
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
						"Id": "container-id",
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

func TestVerifyActualSpecRejectsExtraMountAndPrivileges(t *testing.T) {
	t.Parallel()
	useInit := true
	pidsLimit := workspacePIDs
	base := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{
			Resources:    container.Resources{Memory: 1024, NanoCPUs: workspaceNanoCPUs, PidsLimit: &pidsLimit},
			Init:         &useInit,
			PortBindings: nat.PortMap{nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "49152"}}},
		}},
		Config: &container.Config{Image: "image:test", ExposedPorts: nat.PortSet{nat.Port(workspacePort): struct{}{}}},
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

func TestEnsureRunningUsesOneInspectionForRunningContainer(t *testing.T) {
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
				"Id": "container-id",
				"Config": map[string]any{
					"Image": spec.Image, "Env": sortedEnv(spec.Env),
					"Labels":       map[string]string{managedLabel: "true", workspaceLabel: spec.Name, specFingerprintLabel: fingerprint},
					"ExposedPorts": nat.PortSet{nat.Port(workspacePort): struct{}{}},
				},
				"HostConfig": map[string]any{
					"Memory": spec.MemoryBytes, "NanoCpus": workspaceNanoCPUs, "PidsLimit": workspacePIDs, "Init": useInit,
					"PortBindings":  nat.PortMap{nat.Port(workspacePort): []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}}},
					"RestartPolicy": map[string]any{"Name": "no"},
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
	endpoint, transitioned, err := docker.EnsureRunning(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if transitioned || endpoint.Port != server.Listener.Addr().(*net.TCPAddr).Port {
		t.Fatalf("endpoint=%+v transitioned=%t", endpoint, transitioned)
	}
	if inspections.Load() != 1 {
		t.Fatalf("container inspections = %d, want 1", inspections.Load())
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
				"Id": "created-id",
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
