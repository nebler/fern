package watch

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamControllerReconnectsToChangedEndpoint(t *testing.T) {
	t.Parallel()
	var firstConnections atomic.Int32
	first := eventServer(&firstConnections)
	defer first.Close()
	var secondConnections atomic.Int32
	second := eventServer(&secondConnections)
	defer second.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observations := make(chan Observation, 16)
	controller := NewStreamController(ctx, StreamOptions{}, observations, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stopController(t, controller)
	if err := controller.Connect(ctx, first.URL); err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconnect(ctx, second.URL); err != nil {
		t.Fatal(err)
	}
	if firstConnections.Load() != 1 {
		t.Fatalf("first endpoint connections = %d, want 1", firstConnections.Load())
	}
	if secondConnections.Load() != 1 {
		t.Fatalf("second endpoint connections = %d, want 1", secondConnections.Load())
	}
	var connectedEpoch uint64
	for len(observations) > 0 {
		observation := <-observations
		if observation.Kind == ObservationConnected {
			connectedEpoch = observation.Epoch
		}
	}
	if connectedEpoch != 2 {
		t.Fatalf("latest connected epoch = %d, want 2", connectedEpoch)
	}
}

func TestStreamControllerRecoversAfterFailedConnection(t *testing.T) {
	t.Parallel()
	bad := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	var connections atomic.Int32
	good := eventServer(&connections)
	defer good.Close()

	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	defer serviceCancel()
	controller := NewStreamController(serviceCtx, StreamOptions{}, make(chan Observation, 16), slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stopController(t, controller)
	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := controller.Connect(shortCtx, bad.URL); err == nil {
		t.Fatal("Connect unexpectedly succeeded")
	}
	if err := controller.Connect(context.Background(), good.URL); err != nil {
		t.Fatalf("Connect after failure: %v", err)
	}
	if connections.Load() != 1 {
		t.Fatalf("good endpoint connections = %d, want 1", connections.Load())
	}
}

func TestStreamControllerStopHonorsDeadlineBehindOperation(t *testing.T) {
	t.Parallel()
	controller := NewStreamController(context.Background(), StreamOptions{}, make(chan Observation), testLogger())
	if err := controller.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer controller.release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := controller.Stop(ctx); err == nil {
		t.Fatal("Stop unexpectedly acquired a busy operation gate")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Stop exceeded deadline by too much: %s", elapsed)
	}
}

func stopController(t *testing.T, controller *StreamController) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Stop(ctx); err != nil {
		t.Errorf("stop controller: %v", err)
	}
}

func eventServer(connections *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connections.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
}
