package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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

	mu   sync.Mutex
	gone bool // its box stopped: there is nothing more to read
}

// errBoxStopped is the end of a job whose box went to sleep, was restored
// or deleted.
var errBoxStopped = errors.New("its box went to sleep, was restored or deleted")

// startJob starts in's command in the background in environment id's box,
// and returns its ID.
func (s *Server) startJob(ctx context.Context, id string, in bashInput) (string, error) {
	jobID := "bash_" + strings.ToLower(rand.Text()[:8])
	j, ex, wctx, err := s.openJob(ctx, id, jobID)
	if err != nil {
		return "", err
	}
	if err := j.box.Start(ctx, boxexec.JobRequest{ID: jobID, Argv: []string{"/bin/sh", "-c", in.Command}}); err != nil {
		j.cancel()
		ex.Close()
		return "", err
	}
	s.keepJob(wctx, id, j, ex)
	return jobID, nil
}

// openJob opens a job count in environment id's box, waking it, for job
// jobID, which the caller starts there or found there. The returned context
// is the job's watch's: the box's sleep, restore or delete ends it.
func (s *Server) openJob(ctx context.Context, id, jobID string) (*job, *actor.Exec, context.Context, error) {
	// The watch outlives the call that opens it.
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ex, err := s.envs.Job(ctx, id, "", cancel)
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
	if s.jobs.byEnv == nil {
		s.jobs.byEnv = make(map[string]map[string]*job)
	}
	if s.jobs.byEnv[id] == nil {
		s.jobs.byEnv[id] = make(map[string]*job)
	}
	js := s.jobs.byEnv[id]
	js[j.id] = j
	// The records of jobs whose boxes stopped go, once there are many.
	if len(js) > 2*boxexec.MaxJobs {
		for jid, old := range js {
			if old.isGone() {
				delete(js, jid)
			}
		}
	}
	s.jobs.mu.Unlock()
	s.jobs.wg.Go(func() {
		defer j.cancel()
		defer ex.Close()
		err := j.box.Wait(wctx, j.id)
		switch {
		case s.closing():
			// The daemon stops; the job runs on, for the next one.
		case wctx.Err() != nil || err != nil:
			// Its box stopped, or is gone without its actor knowing yet.
			j.mu.Lock()
			j.gone = true
			j.mu.Unlock()
		}
	})
}

func (j *job) isGone() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.gone
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
	return j.report(j.box.Read(ctx, j.id)), nil
}

// killJob stops environment id's job jobID and returns its last output.
func (s *Server) killJob(ctx context.Context, id, jobID string) (string, error) {
	j, err := s.findJob(id, jobID)
	if err != nil {
		return "", err
	}
	return j.report(j.box.Kill(ctx, j.id)), nil
}

// report formats what reading or killing the job returned for Claude.
func (j *job) report(o boxexec.JobOutput, err error) string {
	if j.isGone() || err != nil {
		return fmt.Sprintf("[%s is stopped: %v]\n", j.id, errBoxStopped)
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
		// Jobs run in environments' own boxes; a golden builder's runs none.
		if state != runsc.Running || store.EnvOfBox(box) != box || setup.IsBuilder(box) {
			continue
		}
		if err := s.adoptJobs(ctx, box); err != nil {
			s.log.WarnContext(ctx, "finding a box's background commands failed", slog.String("env", box), slog.Any("error", err))
		}
	}
}

func (s *Server) adoptJobs(ctx context.Context, id string) error {
	// The box is up: this only learns its address.
	ex, err := s.envs.Exec(ctx, id, "", func() {})
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
