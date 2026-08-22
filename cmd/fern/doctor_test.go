package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidatePhoneTopologyRequiresThreeWayExactMatch(t *testing.T) {
	t.Parallel()
	origin := "https://fern.example.ts.net"
	tests := []struct {
		name       string
		configured string
		asserted   string
		served     string
		local      string
		localErr   error
		wantErr    bool
	}{
		{name: "exact", configured: origin, asserted: origin, served: origin, local: origin},
		{name: "assertion optional", configured: origin, served: origin, local: origin},
		{name: "configuration required", served: origin, local: origin, wantErr: true},
		{name: "assertion slash differs", configured: origin, asserted: origin + "/", served: origin, local: origin, wantErr: true},
		{name: "assertion case differs", configured: origin, asserted: "https://FERN.example.ts.net", served: origin, local: origin, wantErr: true},
		{name: "serve default port differs", configured: origin, served: origin + ":443", local: origin, wantErr: true},
		{name: "serve differs", configured: origin, served: "https://other.example.ts.net", local: origin, wantErr: true},
		{name: "local differs", configured: origin, served: origin, local: "https://other.example.ts.net", wantErr: true},
		{name: "local discovery fails", configured: origin, served: origin, localErr: errors.New("offline"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validatePhoneTopology(test.configured, test.asserted, test.served, test.local, test.localErr)
			if (err != nil) != test.wantErr {
				t.Fatalf("validatePhoneTopology() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestCheckProviderConnection(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "opencode" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"connected"},{"id":"disabled","disabled":true}]}`))
	}))
	defer server.Close()

	count, err := checkProviderConnection(server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestPairingPreviewSendsNoAuthorizationAndDoesNotConsume(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "" || request.Method != http.MethodGet || request.URL.Query().Get("code") != "pair-code" {
			http.Error(writer, "invalid scanner request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, "<h1>Pair this phone?</h1>")
	}))
	defer server.Close()
	if err := checkPairingPreview(server.URL, "pair-code"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("preview requests = %d", requests)
	}
}

func TestCheckRemoteCredentialRequiresUnauthorized(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, _ := request.BasicAuth()
		if username != "opencode" || password != "backend-secret" {
			http.Error(writer, "unexpected credentials", http.StatusBadRequest)
			return
		}
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	if err := checkRemoteCredentialRejected(server.URL, "backend-secret"); err != nil {
		t.Fatal(err)
	}
}

func TestTailscaleTopologyRejectsOperatorListener(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"http://127.0.0.1:8081", "http://localhost:8081", "http://[::1]:8081", "http://0.0.0.0:8081", "http://user@localhost:8081/path"} {
		output := "https://fern.example.ts.net\n|-- / proxy http://127.0.0.1:8080\n\nhttps://operator.example.ts.net\n|-- / proxy " + target + "\n"
		if _, err := tailscaleOriginForTopology(output, "127.0.0.1:8080", "127.0.0.1:8081"); err == nil {
			t.Errorf("accepted a Serve mapping exposing proxy.operatorListen through %s", target)
		}
	}
	remoteOnly := "https://fern.example.ts.net\n|-- / proxy http://127.0.0.1:8080\n"
	if got, err := tailscaleOriginForTopology(remoteOnly, "127.0.0.1:8080", "127.0.0.1:8081"); err != nil || got != "https://fern.example.ts.net" {
		t.Fatalf("remote-only topology = %q, %v", got, err)
	}
}

func TestCheckProviderConnectionRequiresActiveProvider(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"id":"disabled","disabled":true}]}`))
	}))
	defer server.Close()

	if _, err := checkProviderConnection(server.URL, "secret"); err == nil {
		t.Fatal("expected no active provider to fail")
	}
}
