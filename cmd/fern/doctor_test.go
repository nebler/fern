package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestDiagnoseFailsClosedBeforeExternalChecksWithoutSecrets(t *testing.T) {
	t.Parallel()
	report := diagnose(context.Background(), diagnoseOptions{
		ConfigPath: filepath.Join(t.TempDir(), "fern.yaml"),
		EnvPath:    filepath.Join(t.TempDir(), "fern.env"),
	})
	if report.Ready || len(report.Checks) != 1 || report.Checks[0].ID != "secrets" || report.Checks[0].Status != "fail" {
		t.Fatalf("doctor report = %+v", report)
	}
}

func TestBackgroundRouteSurfaceRequiresDedicatedBoundary(t *testing.T) {
	t.Parallel()
	valid := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
	}))
	defer valid.Close()
	if err := checkBackgroundRouteSurface(context.Background(), valid.URL); err != nil {
		t.Fatalf("valid dedicated route: %v", err)
	}

	invalid := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer invalid.Close()
	if err := checkBackgroundRouteSurface(context.Background(), invalid.URL); err == nil {
		t.Fatal("doctor accepted a route without the dedicated boundary")
	}
}
