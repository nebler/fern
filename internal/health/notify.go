package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nebler/fern/internal/api"
)

// NotifiedFile is where a Notifier keeps, in the pool, which problems it has
// told of: a restart of the daemon doesn't tell of them again.
const NotifiedFile = "notified.json"

// The limits of a message: how long its POST may take, how much of one
// problem it says, and how many problems it lists. It is a notification on
// a phone, and `fern doctor` has the rest.
const (
	postTimeout = 10 * time.Second
	maxLine     = 400
	maxListed   = 5
)

// Notifier tells a person who isn't looking at a thread or at the server
// what is wrong on the node: when a problem appears, and when the last one
// has cleared, it POSTs one short text to a URL. The body is the text, which
// is what ntfy.sh and most webhook receivers take.
//
// A problem is told of once while it lasts, whatever its details do
// meanwhile. One that clears beside others is dropped without a word, and
// told of again if it comes back.
type Notifier struct {
	Checker Checker
	URL     string // where the messages go, http or https
	Node    string // the node's name, which starts every message
	// File keeps the problems told of (NotifiedFile in the pool). It is
	// written after a message is sent, in one rename: a daemon that dies in
	// between tells of those problems once more, rather than never.
	File string
	Log  *slog.Logger
}

// CheckURL says what is wrong with a Notifier's URL, if anything.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("want an http or https URL, such as https://ntfy.sh/<topic>")
	}
	return nil
}

// notified is NotifiedFile: the problems told of that still last, by key,
// with when.
type notified struct {
	Sent map[string]time.Time `json:"sent"`
}

// said is which failures of a Notifier's own are in the log already. A
// failure is logged once, and again only after it has worked in between:
// the check runs every minute.
type said struct {
	check, post, save bool
}

// Run looks at the node's problems now and then every `every`, and sends
// what has changed, until ctx is done. Nothing waits for it: a URL that
// doesn't answer costs this goroutine postTimeout a look.
func (n Notifier) Run(ctx context.Context, every time.Duration) {
	// A connection a minute isn't worth keeping open.
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}}
	sent, err := n.read()
	if err != nil {
		// Without it every problem is new: told of once more.
		n.Log.WarnContext(ctx, "the list of problems already told of can't be read; telling of them again", slog.Any("error", err))
	}
	var logged said
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		n.look(ctx, client, sent, &logged)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// look compares the node's problems with those told of, sent, sends a
// message if one is due and brings sent, and its file, up to date.
func (n Notifier) look(ctx context.Context, client *http.Client, sent map[string]time.Time, logged *said) {
	problems, err := n.Checker.Problems(ctx)
	if n.failed(ctx, &logged.check, err, "looking for the node's problems to tell of failed") {
		return
	}
	now := make(map[string]bool, len(problems))
	var fresh []api.Problem
	for _, p := range problems {
		now[key(p)] = true
		if _, told := sent[key(p)]; !told {
			fresh = append(fresh, p)
		}
	}
	before := maps.Clone(sent)
	switch {
	case len(fresh) > 0:
		err := n.post(ctx, client, message(n.Node, fresh))
		if !n.failed(ctx, &logged.post, err, "telling of the node's problems failed; it is tried again at every look") {
			for _, p := range fresh {
				sent[key(p)] = time.Now().UTC().Truncate(time.Second)
			}
		}
	case len(problems) == 0 && len(sent) > 0:
		err := n.post(ctx, client, "Fern on "+n.Node+": nothing is wrong any more that the daemon knows of.")
		if n.failed(ctx, &logged.post, err, "telling of the node's problems failed; it is tried again at every look") {
			return // still to be said: what was told of stays
		}
	}
	maps.DeleteFunc(sent, func(k string, _ time.Time) bool { return !now[k] })
	if maps.Equal(before, sent) {
		return
	}
	n.failed(ctx, &logged.save, n.write(sent), "keeping the list of problems told of failed; a restart of the daemon tells of them again")
}

// failed reports whether err is an error, and logs it with msg unless the
// same step's last failure is in the log already (*logged). A daemon that
// is stopping has nothing to say.
func (n Notifier) failed(ctx context.Context, logged *bool, err error, msg string) bool {
	if err == nil {
		*logged = false
		return false
	}
	if !*logged && ctx.Err() == nil {
		*logged = true
		n.Log.WarnContext(ctx, msg, slog.Any("error", err))
	}
	return true
}

// key names a problem for as long as it lasts: its part, and what it is
// about where the node can have several. Its detail changes meanwhile.
func key(p api.Problem) string {
	if p.About == "" {
		return p.Name
	}
	return p.Name + " " + p.About
}

// message is the text that tells of the new problems on node: one line
// each, and where the rest is.
func message(node string, fresh []api.Problem) string {
	var b strings.Builder
	if len(fresh) == 1 {
		b.WriteString("Fern on " + node + ": " + line(fresh[0]) + "\n")
	} else {
		fmt.Fprintf(&b, "Fern on %s: %d problems\n", node, len(fresh))
		for _, p := range fresh[:min(len(fresh), maxListed)] {
			b.WriteString(line(p) + "\n")
		}
		if more := len(fresh) - maxListed; more > 0 {
			fmt.Fprintf(&b, "and %d more\n", more)
		}
	}
	b.WriteString("`fern doctor` on the server says what to do.")
	return b.String()
}

// line is one problem in a message: its part and the start of its detail.
func line(p api.Problem) string {
	s := p.Name + ": " + p.Detail
	if len(s) > maxLine {
		s = strings.ToValidUTF8(s[:maxLine], "") + "…"
	}
	return s
}

// post sends text to the URL, and fails unless it answers with a 2xx within
// postTimeout. Its error doesn't name the URL, which may be all that keeps
// an ntfy topic private.
func (n Notifier) post(ctx context.Context, client *http.Client, text string) error {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(text))
	if err != nil {
		return errors.New("the URL can't be used")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST: it answered %s", resp.Status)
	}
	return nil
}

// read returns the problems told of, from the file; none if there is no
// file yet.
func (n Notifier) read() (map[string]time.Time, error) {
	data, err := os.ReadFile(n.File)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]time.Time{}, nil
	}
	if err != nil {
		return map[string]time.Time{}, err
	}
	var f notified
	if err := json.Unmarshal(data, &f); err != nil {
		return map[string]time.Time{}, fmt.Errorf("%s: %w", n.File, err)
	}
	if f.Sent == nil {
		f.Sent = map[string]time.Time{}
	}
	return f.Sent, nil
}

// write makes sent the file's content: flushed, then in one rename, so the
// file is the list before or the list after, whatever stops the daemon.
// What a crash leaves beside it (File + ".new") the next write replaces.
func (n Notifier) write(sent map[string]time.Time) error {
	data, err := json.Marshal(notified{Sent: sent})
	if err != nil {
		return err
	}
	next := n.File + ".new"
	f, err := os.OpenFile(next, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(next, n.File); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(n.File))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
