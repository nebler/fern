package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/taskstore"
	"golang.org/x/sync/errgroup"
)

func TestFatalBackgroundFailureBlocksReadiness(t *testing.T) {
	status := observability.NewRegistry()
	status.Healthy(observability.ComponentBackgroundRunSerial)
	group, ctx := errgroup.WithContext(context.Background())
	goComponent(group, ctx, status, observability.ComponentBackgroundRunSerial, func(context.Context) error {
		return taskstore.ErrCorruptStore
	})
	if err := group.Wait(); !errors.Is(err, taskstore.ErrCorruptStore) {
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
