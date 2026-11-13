package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Heartbeat is a sign of life for a watcher outside the server: while the
// daemon runs and its store answers, it GETs a URL every so often, and the
// watcher (healthchecks.io, or any dead man's switch of the same contract)
// tells a person when the GETs stop coming. That is what the Notifier can't
// say: a server that is off, a daemon that doesn't start or hangs, a store
// that no longer answers.
type Heartbeat struct {
	Store Store  // asked for its environments: the beat says it answers
	URL   string // http or https
	Log   *slog.Logger
}

// Run sends a heartbeat now and then every `every`, until ctx is done.
// Nothing waits for it: a URL that doesn't answer costs this goroutine
// postTimeout a beat. A failure is logged once, and again only after a beat
// has gone through in between.
func (h Heartbeat) Run(ctx context.Context, every time.Duration) {
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	logged := false
	for {
		err := h.beat(ctx, client)
		switch {
		case err == nil && logged:
			logged = false
			h.Log.InfoContext(ctx, "the heartbeat goes through again")
		case err != nil && !logged && ctx.Err() == nil:
			logged = true
			h.Log.WarnContext(ctx, "the heartbeat failed; it is tried again at every beat", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// beat asks the store whether it answers and, if it does, GETs the URL. Its
// error doesn't name the URL, whose path is often the check's only secret.
func (h Heartbeat) beat(ctx context.Context, client *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	if _, err := h.Store.List(ctx); err != nil {
		// No beat: the watcher's silence is the message.
		return fmt.Errorf("the store doesn't answer, so no beat is sent: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return errors.New("the URL can't be used")
	}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("GET: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET: it answered %s", resp.Status)
	}
	return nil
}
