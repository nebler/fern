package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/store"
	"golang.org/x/sync/errgroup"
)

func TestFatalBackgroundFailureBlocksReadiness(t *testing.T) {
	status := observability.NewRegistry()
	status.Healthy(observability.ComponentBackgroundRunSerial)
	group, ctx := errgroup.WithContext(context.Background())
	startRunCoordinator(group, &runServices{background: failingService{store.ErrCorruptStore}, status: status}, ctx)
	if err := group.Wait(); !errors.Is(err, store.ErrCorruptStore) {
		t.Fatalf("supervision error = %v", err)
	}
	response := httptest.NewRecorder()
	status.ReadinessHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fern/ready", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d: %s", response.Code, response.Body.String())
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("fatal failure did not cancel sibling services")
	}
}

type failingService struct{ err error }

func (service failingService) Run(context.Context) error { return service.err }
func (failingService) Wake()                             {}
