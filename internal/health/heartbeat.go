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
// daemon runs, its store answers and so does T3, it GETs a URL every so
// often, and the watcher (healthchecks.io, or any dead man's switch of the
// same contract) tells a person when the GETs stop coming. That is what the
// Notifier can't say: a server that is off, a daemon that doesn't start or
// hangs, a store that no longer answers; and, said by both, a T3 that hangs
// or is down.
type Heartbeat struct {
	Store Store  // asked for its environments: the beat says it answers
	T3    *T3    // asked whether it answers; nil: there is no T3
	URL   string // http or https
	Log   *slog.Logger
}

// Why a beat wasn't sent, which the log tells once while it lasts.
var (
	errStore = errors.New("the store doesn't answer, so no beat is sent")
	errT3    = errors.New("T3 doesn't answer, so no beat is sent")
	errGET   = errors.New("the beat's GET failed")
)

// Run sends a heartbeat now and then every `every`, until ctx is done.
// Nothing waits for it: a URL that doesn't answer costs this goroutine
// postTimeout a beat. Why a beat failed is logged once while it lasts, and
// again when it changes or after a beat has gone through in between.
func (h Heartbeat) Run(ctx context.Context, every time.Duration) {
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var logged error // the cause logged last; nil: the beats go through
	for {
		err := h.beat(ctx, client)
		switch {
		case err == nil && logged != nil:
			logged = nil
			h.Log.InfoContext(ctx, "the heartbeat goes through again")
		case err != nil && !errors.Is(err, logged) && ctx.Err() == nil:
			logged = cause(err)
			h.Log.WarnContext(ctx, "the heartbeat failed; it is tried again at every beat", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// cause returns which of errStore, errT3 and errGET err is.
func cause(err error) error {
	for _, c := range []error{errStore, errT3} {
		if errors.Is(err, c) {
			return c
		}
	}
	return errGET
}

// beat asks the store and T3 whether they answer and, if they do, GETs the
// URL. Its error doesn't name the URL, whose path is often the check's only
// secret.
func (h Heartbeat) beat(ctx context.Context, client *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	if _, err := h.Store.List(ctx); err != nil {
		// No beat: the watcher's silence is the message.
		return fmt.Errorf("%w: %w", errStore, err)
	}
	if h.T3 != nil {
		if err := h.T3.answer(ctx); err != nil {
			return fmt.Errorf("%w: %w", errT3, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return fmt.Errorf("%w: the URL can't be used", errGET)
	}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("%w: %w", errGET, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: it answered %s", errGET, resp.Status)
	}
	return nil
}
