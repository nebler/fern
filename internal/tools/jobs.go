package tools

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/background"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// jobs are the background commands Claude started, by environment. Each runs
// in its box's fern-init (background.Jobs), which keeps it and its output, so it
// outlives the Bash call, Claude's hibernation and the daemon's restarts
// (AdoptJobs); it ends with KillShell, or with its box's sleep, restore or
// delete. While it runs, the daemon counts it as a job of its box's actor
// (actor.Group.Job), so the box sleeps on its idle clocks.
type jobs struct {
	envs     *actor.Group
	driver   Driver
	boxes    *http.Client // asks boxes' fern-init for their jobs
	log      *slog.Logger
	stopping func() bool // whether the daemon has begun to stop

	mu     sync.Mutex
	byEnv  map[string]map[string]*job
	closed bool           // close has begun
	wg     sync.WaitGroup // the watches
}

// job is the daemon's record of a background command.
type job struct {
	id     string
	box    background.JobClient
	cancel context.CancelFunc // ends the watch of the job, not the job
	done   chan struct{}      // closed when the job's watch has ended, its state set
	state  atomic.Int32       // a jobState, set once by the job's watch as it ends
	killed atomic.Bool        // KillShell stopped it: the agent's call, after which the box lingers as after a Bash call

	// mu guards last: what an exited job had left to say, which its watch
	// took from the box as it ended. The box sleeps 5 s after Claude's next
	// Bash call and forgets its jobs, and Claude often asks later than that.
	mu   sync.Mutex
	last *background.JobOutput
}

// keep records what the job had left to say as it ended.
func (j *job) keep(o background.JobOutput) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.last = &o
}

// kept returns what an exited job had left to say, its output once, and
// whether its watch kept it.
func (j *job) kept() (background.JobOutput, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.last == nil {
		return background.JobOutput{}, false
	}
	o := *j.last
	j.last.Output, j.last.Dropped = "", 0
	return o, true
}

// read reports whether Claude has read all an exited job had to say.
func (j *job) read() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.last != nil && j.last.Output == ""
}

// jobState is what the daemon knows of a background command. It leaves
// jobWatching once, when the job's watch ends, for the state that says why.
type jobState int32

const (
	// jobWatching: it runs, as far as the daemon knows, and its box's actor
	// counts it.
	jobWatching jobState = iota
	// jobExited: it ended by itself, or by KillShell. The daemon has what it
	// had left to say (job.last), or its box does.
	jobExited
	// jobBoxStopped: its box went to sleep, was restored or deleted. There
	// is nothing more to read.
	jobBoxStopped
	// jobLost: its watch failed with the box perhaps still up. The box is
	// asked all the same.
	jobLost
)

func (j *job) is(s jobState) bool { return jobState(j.state.Load()) == s }

// errBoxStopped is the end of a job whose box went to sleep, was restored
// or deleted.
var errBoxStopped = errors.New("its box went to sleep, was restored or deleted")

// startJob starts in's command in the background in environment id's box,
// and returns its ID.
func (js *jobs) startJob(ctx context.Context, id string, in bashInput, key string) (string, error) {
	// A call sent again after a restart names the same job, and doesn't start
	// a second one.
	if key == "" {
		key = rand.Text()
	}
	// The relay's keys are rand.Text's. One from anywhere else must not
	// become part of a path, or end the daemon for being short.
	if len(key) < 8 || strings.ContainsFunc(key[:8], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) || r > unicode.MaxASCII }) {
		return "", errors.New("the call's key is not one the relay makes")
	}
	jobID := "bash_" + strings.ToLower(key[:8])
	if js.hasJob(id, jobID) {
		return jobID, nil
	}
	j, ex, wctx, err := js.openJob(ctx, id, jobID)
	if err != nil {
		return "", err
	}
	err = j.box.Start(ctx, background.JobRequest{ID: jobID, Argv: []string{shell, "-c", in.Command}})
	if err != nil && !errors.Is(err, background.ErrJobExists) {
		j.cancel()
		ex.Close()
		return "", err
	}
	js.keepJob(wctx, id, j, ex)
	return jobID, nil
}

// hasJob reports whether the daemon knows job jobID of environment id.
func (js *jobs) hasJob(id, jobID string) bool {
	js.mu.Lock()
	defer js.mu.Unlock()
	_, ok := js.byEnv[id][jobID]
	return ok
}

// openJob opens a job count in environment id's box, waking it, for job
// jobID, which the caller starts there or found there. The returned context
// is the job's watch's: the box's sleep, restore or delete ends it.
func (js *jobs) openJob(ctx context.Context, id, jobID string) (*job, *actor.Exec, context.Context, error) {
	// The watch outlives the call that opens it.
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ex, err := js.envs.Job(ctx, id, cancel)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	j := &job{id: jobID, box: background.JobClient{HC: js.boxes, Addr: ex.Addr, Token: ex.Token}, cancel: cancel, done: make(chan struct{})}
	return j, ex, wctx, nil
}

// keepJob records job j of environment id and watches it until it ends or
// its box stops (wctx is done), then closes its count ex.
func (js *jobs) keepJob(wctx context.Context, id string, j *job, ex *actor.Exec) {
	js.mu.Lock()
	// One watch a job: a call sent again, or an adoption, may have raced the
	// one that keeps it. And none once the watches are being waited for.
	if old, ok := js.byEnv[id][j.id]; js.closed || ok && old.is(jobWatching) {
		js.mu.Unlock()
		j.cancel()
		ex.Close()
		return
	}
	if js.byEnv == nil {
		js.byEnv = make(map[string]map[string]*job)
	}
	if js.byEnv[id] == nil {
		js.byEnv[id] = make(map[string]*job)
	}
	ofEnv := js.byEnv[id]
	ofEnv[j.id] = j
	// The records of jobs there is nothing more to read of go, once there
	// are many; then those of ended jobs nobody read, which hold their output.
	if len(ofEnv) > 2*background.MaxJobs {
		for jid, old := range ofEnv {
			if old.is(jobBoxStopped) || old.is(jobLost) || old.is(jobExited) && old.read() {
				delete(ofEnv, jid)
			}
		}
		for jid, old := range ofEnv {
			if len(ofEnv) <= 2*background.MaxJobs {
				break
			}
			if old.is(jobExited) {
				delete(ofEnv, jid)
			}
		}
	}
	js.mu.Unlock()
	js.wg.Go(func() {
		defer j.cancel()
		defer close(j.done)
		defer func() {
			if j.killed.Load() {
				ex.CloseAndSleep(bashLinger)
			} else {
				ex.Close()
			}
		}()
		err := j.box.Wait(wctx, j.id)
		switch {
		case js.closing():
			// The daemon stops; the job runs on, for the next one.
		case wctx.Err() != nil:
			j.state.Store(int32(jobBoxStopped))
		case err != nil:
			// Its box may be gone without its actor knowing yet.
			j.state.Store(int32(jobLost))
		default:
			// The job still counts, so its box is up. A box that says the job
			// still runs is on its way down, with an older fern-init whose wait
			// ended without saying why: the job didn't end by itself.
			o, err := j.box.Read(wctx, j.id)
			if err == nil && o.Running {
				j.state.Store(int32(jobLost))
				break
			}
			if err == nil {
				j.keep(o)
			}
			j.state.Store(int32(jobExited))
		}
	})
}

// findJob returns environment id's job jobID.
func (js *jobs) findJob(id, jobID string) (*job, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if j, ok := js.byEnv[id][jobID]; ok {
		return j, nil
	}
	return nil, fmt.Errorf("no background command %s: the daemon keeps them in memory, so one is forgotten when the daemon restarts after its box has slept. "+
		"If it wrote to a file in the worktree, read that", jobID)
}

// readJob returns environment id's job jobID's output since the last read,
// and whether it runs, once it has ended or wait has passed, whichever is
// first. Someone waiting counts as a command of the box, so the box stays
// up meanwhile: a job alone doesn't hold it past its idle times, and a build
// Claude waits ten minutes for would end with the box's sleep after five.
func (js *jobs) readJob(ctx context.Context, id, jobID string, wait time.Duration) (string, error) {
	j, err := js.findJob(id, jobID)
	if err != nil {
		return "", err
	}
	if wait > 0 && j.is(jobWatching) {
		wctx, cancel := context.WithTimeout(ctx, min(wait, bashMaxTimeout))
		if ex, err := js.envs.Exec(wctx, id, cancel); err == nil {
			select {
			case <-j.done:
			case <-wctx.Done():
			}
			ex.CloseAndSleep(bashLinger)
		}
		cancel()
		// A daemon that stops ends the watch, not the job: this call's answer
		// goes nowhere (MCPHandler), and reading the box now would take the
		// output from the call the relay sends the next daemon.
		if js.stopping() {
			return "", errors.New("the daemon is restarting")
		}
	}
	if j.is(jobBoxStopped) {
		return j.stopped(), nil // without a call to a box that isn't there
	}
	// Its state first: the watch keeps the output before it says it ended.
	if j.is(jobExited) {
		if o, ok := j.kept(); ok {
			return j.report(o, nil), nil // its box may be asleep by now
		}
	}
	o, err := j.box.Read(ctx, j.id)
	if errors.Is(err, background.ErrNoJob) {
		js.forgetJob(id, j)
	}
	return j.report(o, err), nil
}

// killJob stops environment id's job jobID and returns its last output.
func (js *jobs) killJob(ctx context.Context, id, jobID string) (string, error) {
	j, err := js.findJob(id, jobID)
	if err != nil {
		return "", err
	}
	if j.is(jobBoxStopped) {
		return j.stopped(), nil
	}
	if j.is(jobExited) {
		if o, ok := j.kept(); ok {
			return j.report(o, nil), nil
		}
	}
	j.killed.Store(true)
	o, err := j.box.Kill(ctx, j.id)
	if errors.Is(err, background.ErrNoJob) {
		js.forgetJob(id, j)
	}
	return j.report(o, err), nil
}

// forgetJob drops the record of j, a job its box no longer has: the box
// keeps only so many finished ones.
func (js *jobs) forgetJob(id string, j *job) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.byEnv[id][j.id] == j && !j.is(jobWatching) {
		delete(js.byEnv[id], j.id)
	}
}

// stopped is what Claude reads of a job whose box stopped.
func (j *job) stopped() string {
	return fmt.Sprintf("[%s is stopped: %v]\n", j.id, errBoxStopped)
}

// report formats what reading or killing the job returned for Claude.
func (j *job) report(o background.JobOutput, err error) string {
	switch {
	case errors.Is(err, background.ErrNoJob) && !j.is(jobBoxStopped):
		return fmt.Sprintf("[%s has ended, and its box no longer has it: a box keeps its newest %d, and none over a sleep]\n", j.id, background.MaxJobs)
	case err != nil || j.is(jobBoxStopped):
		return j.stopped()
	}
	var status string
	switch {
	case o.Running:
		status = "running"
	case o.Killed:
		status = "killed"
	default:
		status = fmt.Sprintf("exited with code %d", o.Code)
	}
	text := fmt.Sprintf("[%s is %s]\n", j.id, status)
	if o.Dropped > 0 {
		text += fmt.Sprintf("[… %d bytes of output dropped …]\n", o.Dropped)
	}
	return text + clip(o.Output)
}

// adoptAll finds the background commands running in the boxes an earlier
// daemon left awake, so that BashOutput and KillShell reach them and their
// boxes stay up on their idle clocks. It returns when it has asked every
// box, or ctx is done.
func (js *jobs) adoptAll(ctx context.Context) {
	states, err := js.driver.States(ctx)
	if err != nil {
		js.log.WarnContext(ctx, "finding the boxes' background commands failed", slog.Any("error", err))
		return
	}
	for box, state := range states {
		if state != runsc.Running || store.IsBuilder(box) {
			continue
		}
		// Jobs run in environments' own boxes.
		if store.EnvOfBox(box) == box {
			if err := js.adoptJobs(ctx, box); err != nil {
				js.log.WarnContext(ctx, "finding a box's background commands failed", slog.String("env", box), slog.Any("error", err))
			}
		}
		// And its idle clocks go on from where the last daemon's were.
		js.envs.AdoptClocks(ctx, box)
	}
}

func (js *jobs) adoptJobs(ctx context.Context, id string) error {
	// The box is up: this only learns its address.
	ex, err := js.envs.Exec(ctx, id, func() {})
	if err != nil {
		return err
	}
	infos, err := background.JobClient{HC: js.boxes, Addr: ex.Addr, Token: ex.Token}.List(ctx)
	ex.Close()
	if err != nil {
		return err
	}
	for _, info := range infos {
		j, jex, wctx, err := js.openJob(ctx, id, info.ID)
		if err != nil {
			return err
		}
		js.keepJob(wctx, id, j, jex)
		js.log.InfoContext(ctx, "background command adopted", slog.String("env", id), slog.String("job", info.ID),
			slog.Bool("running", info.Running))
	}
	return nil
}

// closing reports whether close has begun: watches that end then end with
// the daemon, not with their boxes.
func (js *jobs) closing() bool {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.closed
}

// close stops watching the background commands, which run on in their
// boxes for the next daemon to adopt, and waits for the watches to end.
func (js *jobs) close() {
	js.mu.Lock()
	js.closed = true
	for _, ofEnv := range js.byEnv {
		for _, j := range ofEnv {
			j.cancel()
		}
	}
	js.mu.Unlock()
	js.wg.Wait()
}

// AdoptJobs finds the background commands running in the boxes an earlier
// daemon left awake (jobs.adoptAll). It returns when it has asked every box,
// or ctx is done.
func (s *Server) AdoptJobs(ctx context.Context) { s.jobs.adoptAll(ctx) }

// Close stops watching the background commands, which run on in their boxes
// for the next daemon to adopt, and waits for the watches to end.
func (s *Server) Close() { s.jobs.close() }
