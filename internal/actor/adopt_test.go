package actor_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// A new daemon gives a box's idle clocks back the times its fern-init kept,
// so the box sleeps when it would have without the restart. A box that runs
// a command keeps the clocks the new daemon started.
func TestAdoptClocks(t *testing.T) {
	opened := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	closed := opened.Add(8 * time.Minute)
	tests := []struct {
		name    string
		running int
		adopted bool
	}{
		{"an idle box", 0, true},
		{"a box running a command", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != boxexec.ActivityPath || r.Header.Get("Authorization") != "Bearer "+runsc.FakeToken {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(boxexec.Activity{Opened: opened, Closed: closed, Running: tt.running})
			}))
			t.Cleanup(box.Close)
			st, drv := newFakeStore(), runsc.NewFake(box.Listener.Addr().String())
			g := start(t, st, drv, idle)
			ctx := t.Context()
			if err := g.Create(ctx, "demo", store.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := g.Wake(ctx, "demo"); err != nil {
				t.Fatal(err)
			}
			g.AdoptClocks(ctx, "demo")
			h, err := g.Holds(ctx, "demo")
			if err != nil {
				t.Fatalf("Holds(demo) error: %v", err)
			}
			// The person's clock is the later one: 20 minutes after the last
			// command started.
			if got := h.SleepAt.Equal(opened.Add(idle.Human)); got != tt.adopted {
				t.Errorf("after AdoptClocks the box sleeps at %s; at the box's own time, %s: %t, want %t",
					h.SleepAt, opened.Add(idle.Human), got, tt.adopted)
			}
		})
	}
}
