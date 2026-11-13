package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/api"
)

// t3Timeout is how long T3 has to answer: on the node, it answers in
// milliseconds when it runs.
const t3Timeout = 5 * time.Second

// t3Down is how long T3 must have failed to answer before it is a problem:
// an update or a crash restarts it in seconds, and a look that met such a
// restart would tell the person of a fault that was already gone.
const t3Down = 30 * time.Second

// T3 is T3's server on the node. The daemon can be fine while T3 hangs or
// is down, and every thread with it: a Heartbeat sends no beat then, and it
// is a problem.
type T3 struct {
	URL string // T3's server, such as http://127.0.0.1:30773
	// Grace is how long it must fail to answer before it is a problem; 0
	// means t3Down.
	Grace time.Duration

	mu        sync.Mutex
	downSince time.Time // when it first failed to answer, since it last did; zero while it answers
}

// answer GETs T3's /.well-known/t3/environment, which it answers while it
// runs, and returns why it didn't answer, or nil.
func (t *T3) answer(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, t3Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(t.URL, "/")+"/.well-known/t3/environment", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("it answered %s", resp.Status)
	}
	return nil
}

// problem returns the problem of a T3 that doesn't answer, if it hasn't
// for t3Down: one that is only restarting is none.
func (t *T3) problem(ctx context.Context) (api.Problem, bool) {
	err := t.answer(ctx)
	t.mu.Lock()
	now := time.Now()
	switch {
	case err == nil:
		t.downSince = time.Time{}
	case t.downSince.IsZero():
		t.downSince = now
	}
	since := t.downSince
	t.mu.Unlock()
	grace := t.Grace
	if grace == 0 {
		grace = t3Down
	}
	if err == nil || now.Sub(since) < grace {
		return api.Problem{}, false
	}
	return api.Problem{
		Name:   "t3",
		Detail: fmt.Sprintf("T3 doesn't answer at %s, so no thread works: %s", t.URL, firstLine(err.Error())),
		Fix:    "`systemctl status fern-t3` and `journalctl -u fern-t3` say why; `systemctl restart fern-t3` starts it again",
	}, true
}
