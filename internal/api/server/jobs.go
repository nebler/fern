package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// jobs are the background commands Claude started, by environment. Each runs
// in its box's fern-init (boxexec.Jobs), which keeps it and its output, so it
// outlives the Bash call, Claude's hibernation and the daemon's restarts
// (AdoptJobs); it ends with KillShell, or with its box's sleep, restore or
// delete. While it runs, the daemon counts it as a job of its box's actor
// (actor.Group.Job), so the box sleeps on its idle clocks.
type jobs struct {
	mu     sync.Mutex
	byEnv  map[string]map[string]*job
	closed bool           // Close has begun
	wg     sync.WaitGroup // the watches
}

// job is the daemon's record of a background command.
type job struct {
	id     string
	box    boxexec.JobClient
	cancel context.CancelFunc // ends the watch of the job, not the job
	state  atomic.Int32       // a jobState, set once by the job's watch as it ends
	killed atomic.Bool        // KillShell stopped it: the agent's call, after which the box lingers as after a Bash call
}

// jobState is what the daemon knows of a background command.
type jobState int32

const (
	// jobWatching: it runs, as far as the daemon knows, and its box's actor
	// counts it.
	jobWatching jobState = iota
	// jobExited: it ended by itself, or by KillShell. Its box has its output.
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
func (s *Server) startJob(ctx context.Context, id string, in bashInput, key string) (string, error) {
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
	if s.hasJob(id, jobID) {
		return jobID, nil
	}
	j, ex, wctx, err := s.openJob(ctx, id, jobID)
	if err != nil {
		return "", err
	}
	err = j.box.Start(ctx, boxexec.JobRequest{ID: jobID, Argv: []string{"/bin/sh", "-c", in.Command}})
	if err != nil && !errors.Is(err, boxexec.ErrJobExists) {
		j.cancel()
		ex.Close()
		return "", err
	}
	s.keepJob(wctx, id, j, ex)
	return jobID, nil
}

// hasJob reports whether the daemon knows job jobID of environment id.
func (s *Server) hasJob(id, jobID string) bool {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	_, ok := s.jobs.byEnv[id][jobID]
	return ok
}

// openJob opens a job count in environment id's box, waking it, for job
// jobID, which the caller starts there or found there. The returned context
// is the job's watch's: the box's sleep, restore or delete ends it.
func (s *Server) openJob(ctx context.Context, id, jobID string) (*job, *actor.Exec, context.Context, error) {
	// The watch outlives the call that opens it.
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ex, err := s.envs.Job(ctx, id, cancel)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	j := &job{id: jobID, box: boxexec.JobClient{HC: s.boxes, Addr: ex.Addr, Token: ex.Token}, cancel: cancel}
	return j, ex, wctx, nil
}

// keepJob records job j of environment id and watches it until it ends or
// its box stops (wctx is done), then closes its count ex.
func (s *Server) keepJob(wctx context.Context, id string, j *job, ex *actor.Exec) {
	s.jobs.mu.Lock()
	// One watch a job: a call sent again, or an adoption, may have raced the
	// one that keeps it. And none once the watches are being waited for.
	if old, ok := s.jobs.byEnv[id][j.id]; s.jobs.closed || ok && old.is(jobWatching) {
		s.jobs.mu.Unlock()
		j.cancel()
		ex.Close()
		return
	}
	if s.jobs.byEnv == nil {
		s.jobs.byEnv = make(map[string]map[string]*job)
	}
	if s.jobs.byEnv[id] == nil {
		s.jobs.byEnv[id] = make(map[string]*job)
	}
	js := s.jobs.byEnv[id]
	js[j.id] = j
	// The records of jobs there is nothing more to read of go, once there
	// are many.
	if len(js) > 2*boxexec.MaxJobs {
		for jid, old := range js {
			if old.is(jobBoxStopped) || old.is(jobLost) {
				delete(js, jid)
			}
		}
	}
	s.jobs.mu.Unlock()
	s.jobs.wg.Go(func() {
		defer j.cancel()
		defer func() {
			if j.killed.Load() {
				ex.CloseAndSleep(bashLinger)
			} else {
				ex.Close()
			}
		}()
		err := j.box.Wait(wctx, j.id)
		switch {
		case s.closing():
			// The daemon stops; the job runs on, for the next one.
		case wctx.Err() != nil:
			j.state.Store(int32(jobBoxStopped))
		case err != nil:
			// Its box may be gone without its actor knowing yet.
			j.state.Store(int32(jobLost))
		default:
			j.state.Store(int32(jobExited))
		}
	})
}

// findJob returns environment id's job jobID.
func (s *Server) findJob(id, jobID string) (*job, error) {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	if j, ok := s.jobs.byEnv[id][jobID]; ok {
		return j, nil
	}
	return nil, fmt.Errorf("no background command %s", jobID)
}

// readJob returns environment id's job jobID's output since the last read,
// and whether it runs.
func (s *Server) readJob(ctx context.Context, id, jobID string) (string, error) {
	j, err := s.findJob(id, jobID)
	if err != nil {
		return "", err
	}
	if j.is(jobBoxStopped) {
		return j.stopped(), nil // without a call to a box that isn't there
	}
	o, err := j.box.Read(ctx, j.id)
	if errors.Is(err, boxexec.ErrNoJob) {
		s.forgetJob(id, j)
	}
	return j.report(o, err), nil
}

// killJob stops environment id's job jobID and returns its last output.
func (s *Server) killJob(ctx context.Context, id, jobID string) (string, error) {
	j, err := s.findJob(id, jobID)
	if err != nil {
		return "", err
	}
	if j.is(jobBoxStopped) {
		return j.stopped(), nil
	}
	j.killed.Store(true)
	o, err := j.box.Kill(ctx, j.id)
	if errors.Is(err, boxexec.ErrNoJob) {
		s.forgetJob(id, j)
	}
	return j.report(o, err), nil
}

// forgetJob drops the record of j, a job its box no longer has: the box
// keeps only so many finished ones.
func (s *Server) forgetJob(id string, j *job) {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	if s.jobs.byEnv[id][j.id] == j && !j.is(jobWatching) {
		delete(s.jobs.byEnv[id], j.id)
	}
}

// stopped is what Claude reads of a job whose box stopped.
func (j *job) stopped() string {
	return fmt.Sprintf("[%s is stopped: %v]\n", j.id, errBoxStopped)
}

// report formats what reading or killing the job returned for Claude.
func (j *job) report(o boxexec.JobOutput, err error) string {
	switch {
	case errors.Is(err, boxexec.ErrNoJob) && !j.is(jobBoxStopped):
		return fmt.Sprintf("[%s has ended, and its box has dropped it to make room for newer ones]\n", j.id)
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

// AdoptJobs finds the background commands running in the boxes an earlier
// daemon left awake, so that BashOutput and KillShell reach them and their
// boxes stay up on their idle clocks. It returns when it has asked every
// box, or ctx is done.
func (s *Server) AdoptJobs(ctx context.Context) {
	states, err := s.driver.States(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "finding the boxes' background commands failed", slog.Any("error", err))
		return
	}
	for box, state := range states {
		if state != runsc.Running || setup.IsBuilder(box) {
			continue
		}
		// Jobs run in environments' own boxes.
		if store.EnvOfBox(box) == box {
			if err := s.adoptJobs(ctx, box); err != nil {
				s.log.WarnContext(ctx, "finding a box's background commands failed", slog.String("env", box), slog.Any("error", err))
			}
		}
		s.adoptClocks(ctx, box)
	}
}

// adoptClocks gives box's idle clocks back the times its fern-init kept, so
// that a restart doesn't keep idle boxes awake longer. A box running a
// command, perhaps one waiting for its client to come back, keeps new ones.
func (s *Server) adoptClocks(ctx context.Context, box string) {
	ex, err := s.envs.Exec(ctx, box, func() {})
	if err != nil {
		return
	}
	a, err := boxexec.ActivityOf(ctx, s.boxes, ex.Addr, ex.Token)
	ex.Close()
	if err != nil || a.Running > 0 {
		return // an older fern-init has none; the clocks just started over
	}
	if err := s.envs.AdoptClocks(ctx, box, a.Opened, a.Closed); err != nil {
		s.log.WarnContext(ctx, "restoring a box's idle clocks failed", slog.String("env", box), slog.Any("error", err))
	}
}

func (s *Server) adoptJobs(ctx context.Context, id string) error {
	// The box is up: this only learns its address.
	ex, err := s.envs.Exec(ctx, id, func() {})
	if err != nil {
		return err
	}
	infos, err := boxexec.JobClient{HC: s.boxes, Addr: ex.Addr, Token: ex.Token}.List(ctx)
	ex.Close()
	if err != nil {
		return err
	}
	for _, info := range infos {
		j, jex, wctx, err := s.openJob(ctx, id, info.ID)
		if err != nil {
			return err
		}
		s.keepJob(wctx, id, j, jex)
		s.log.InfoContext(ctx, "background command adopted", slog.String("env", id), slog.String("job", info.ID),
			slog.Bool("running", info.Running))
	}
	return nil
}

// closing reports whether Close has begun: watches that end then end with
// the daemon, not with their boxes.
func (s *Server) closing() bool {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	return s.jobs.closed
}

// Close stops watching the background commands, which run on in their
// boxes for the next daemon to adopt, and waits for the watches to end.
func (s *Server) Close() {
	s.jobs.mu.Lock()
	s.jobs.closed = true
	for _, js := range s.jobs.byEnv {
		for _, j := range js {
			j.cancel()
		}
	}
	s.jobs.mu.Unlock()
	s.jobs.wg.Wait()
}
