package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/runtime"
)

func TestLegacyWorkflowAndPublicationRoutesAreAuthenticatedGoneWithoutMutation(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	waker := &countingWaker{}
	handler := NewWithControls(waker, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger())
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/fern/api/v1/workflows"},
		{http.MethodPost, "/fern/api/v1/workflows"},
		{http.MethodGet, "/fern/api/v1/workflows/workflow-1"},
		{http.MethodPost, "/fern/api/v1/workflows/workflow-1/publish"},
		{http.MethodGet, "/fern/api/v1/publications"},
		{http.MethodPost, "/fern/workflows"},
		{http.MethodPost, "/fern/workflows/workflow-1/publish"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(`{"title":"must not persist"}`))
		request.SetBasicAuth("fern", "control-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusGone {
			t.Errorf("%s %s status=%d body=%q", test.method, test.path, response.Code, response.Body.String())
		}
	}
	if len(store.Workflows()) != 0 || len(store.Publications()) != 0 || waker.wakes.Load() != 0 {
		t.Fatalf("retired routes mutated or woke state: workflows=%d publications=%d wakes=%d", len(store.Workflows()), len(store.Publications()), waker.wakes.Load())
	}
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/fern/api/v1/publications", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated retired route status=%d", unauthenticated.Code)
	}
	for _, path := range []string{"/fern/api/v1/workflows/workflow-1/other", "/fern/api/v1/workflows//publish", "/fern/workflows/workflow-1", "/fern/api/v1/publications/one"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.SetBasicAuth("fern", "control-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Errorf("nearby non-route %s status=%d", path, response.Code)
		}
	}
}

func TestControlPageDoesNotRenderLegacyWorkflowOrPublicationUI(t *testing.T) {
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/fern/control", nil)
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	NewWithControls(nil, runtime.ServerAuth{Password: "secret"}, Controls{Store: store, ControlAuth: ControlAuth{Password: "control-secret"}}, testLogger()).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	for _, retired := range []string{"Tracked work", "Track OpenCode session", "Publish draft PR", "Retry publication", "<h2>Publications</h2>"} {
		if strings.Contains(response.Body.String(), retired) {
			t.Errorf("control page retained %q", retired)
		}
	}
}

func TestControlMutationOriginPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		origin    string
		host      string
		fetchSite string
		want      bool
	}{
		{name: "non-browser client", host: "fern.example", want: true},
		{name: "same HTTPS origin", origin: "https://fern.example", host: "fern.example", fetchSite: "same-origin", want: true},
		{name: "same HTTP origin", origin: "http://127.0.0.1:8080", host: "127.0.0.1:8080", want: true},
		{name: "cross origin", origin: "https://evil.example", host: "fern.example"},
		{name: "same site sibling", origin: "https://fern.example", host: "fern.example", fetchSite: "same-site"},
		{name: "cross site metadata", host: "fern.example", fetchSite: "cross-site"},
		{name: "opaque metadata", host: "fern.example", fetchSite: "none"},
		{name: "opaque origin", origin: "null", host: "fern.example"},
		{name: "userinfo", origin: "https://user@fern.example", host: "fern.example"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://"+test.host+"/fern/workflows", nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			if got := sameOrigin(request); got != test.want {
				t.Fatalf("sameOrigin() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestOperatorMutationUsesTrustedOriginNotClientHost(t *testing.T) {
	t.Parallel()
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	trustedDevice, err := store.AddDevice("trusted", "Trusted", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rejectedDevice, err := store.AddDevice("rejected", "Rejected", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := NewHandlers(nil, runtime.ServerAuth{Password: "backend-secret"}, Controls{
		Store: store, ControlAuth: ControlAuth{Password: "control-secret"},
	}, TrustedOrigins{Remote: "https://fern.example.ts.net", Operator: "http://127.0.0.1:8081"}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/fern/api/v1/devices/"+trustedDevice.ID, nil)
	request.Host = "spoofed.example"
	request.Header.Set("Origin", "http://127.0.0.1:8081")
	request.SetBasicAuth("fern", "control-secret")
	response := httptest.NewRecorder()
	handlers.Operator.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("trusted operator mutation status=%d body=%q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodDelete, "/fern/api/v1/devices/"+rejectedDevice.ID, nil)
	request.Host = "127.0.0.1:8081"
	request.Header.Set("Origin", "http://spoofed.example")
	request.SetBasicAuth("fern", "control-secret")
	response = httptest.NewRecorder()
	handlers.Operator.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin operator mutation status=%d", response.Code)
	}
	if _, valid, err := store.AuthenticateDeviceIdentity("rejected", now); err != nil || !valid {
		t.Fatalf("cross-origin mutation changed device: valid=%t err=%v", valid, err)
	}
}

func TestRevocationCallbackRunsOnlyAfterSuccessfulPersistence(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "control")
	store, err := control.Open(directory, "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	device, err := store.AddDevice("first", "First", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	callbackIDs := make(chan string, 2)
	callback := func(id string) {
		if _, valid, authErr := store.AuthenticateDeviceIdentity("first", now); authErr != nil || valid {
			t.Errorf("callback ran before durable removal: valid=%t err=%v", valid, authErr)
		}
		callbackIDs <- id
	}
	if err := revokeDevice(store, device.ID, callback); err != nil {
		t.Fatal(err)
	}
	if got := <-callbackIDs; got != device.ID {
		t.Fatalf("callback ID=%q, want %q", got, device.ID)
	}
	if err := revokeDevice(store, device.ID, callback); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second revocation error=%v", err)
	}
	select {
	case id := <-callbackIDs:
		t.Fatalf("failed revocation invoked callback for %q", id)
	default:
	}

	second, err := store.AddDevice("second", "Second", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	moved := directory + "-moved"
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := revokeDevice(store, second.ID, callback); err == nil {
		t.Fatal("revocation unexpectedly persisted with an unavailable state directory")
	}
	select {
	case id := <-callbackIDs:
		t.Fatalf("persistence failure invoked callback for %q", id)
	default:
	}
}

func TestDeviceRegistrationRevocationRaceIsFenced(t *testing.T) {
	store, err := control.Open(filepath.Join(t.TempDir(), "control"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for iteration := range 100 {
		token := "device-" + string(rune(iteration+1))
		device, err := store.AddDevice(token, "Racing device", now, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		registered := make(chan bool, 1)
		go func() {
			<-start
			_, admitted := store.RegisterDeviceRequest(device.ID, cancel)
			registered <- admitted
		}()
		revoked := make(chan error, 1)
		go func() {
			<-start
			revoked <- revokeDevice(store, device.ID, store.CancelDeviceRequests)
		}()
		close(start)
		admitted := <-registered
		if err := <-revoked; err != nil {
			t.Fatal(err)
		}
		if !admitted {
			cancel()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatalf("iteration %d left an admitted request alive", iteration)
		}
	}
}
