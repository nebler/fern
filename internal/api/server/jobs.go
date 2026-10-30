package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/nebler/fern/internal/boxexec"
)

// Limits of Claude's background commands (Bash's run_in_background): how
// many an environment keeps, finished ones included, and how much unread
// output one keeps, its newest.
const (
	maxJobs      = 16
	maxJobOutput = 1 << 20
)

// jobs are the background commands Claude started, by environment. Each
// runs as a command in the environment's box that the daemon holds open
// (actor.Group.Job), so it outlives the Bash call, and Claude's own
// hibernation: it ends with KillShell, with its box's sleep, restore or
// delete, or with the daemon (Close).
type jobs struct {
	mu    sync.Mutex
	next  int
	byEnv map[string][]*job // oldest first
	wg    sync.WaitGroup
}

// job is one background command. Its fields below mu change as it runs.
type job struct {
	id      string
	command string
	cancel  context.CancelFunc
	ended   chan struct{} // closed once the command's exec has ended

	mu      sync.Mutex
	out     []byte // output not read yet, stdout and stderr together
	dropped int    // bytes of it dropped, the oldest, past maxJobOutput
	done    bool
	killed  bool // by KillShell
	code    int
	err     error
}

// Write keeps p as unread output.
func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.out = append(j.out, p...)
	if over := len(j.out) - maxJobOutput; over > 0 {
		j.out = append(j.out[:0], j.out[over:]...)
		j.dropped += over
	}
	return len(p), nil
}

// read returns the output since the last read and the job's status.
func (j *job) read() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var status string
	switch {
	case !j.done:
		status = "running"
	case j.killed:
		status = "killed"
	case j.err != nil:
		status = "stopped: " + j.err.Error()
	default:
		status = fmt.Sprintf("exited with code %d", j.code)
	}
	text := fmt.Sprintf("[%s is %s]\n", j.id, status)
	if j.dropped > 0 {
		text += fmt.Sprintf("[… %d bytes of output dropped …]\n", j.dropped)
	}
	text += clip(string(j.out))
	j.out, j.dropped = nil, 0
	return text
}

// errBoxStopped is the end of a job whose box went to sleep, was restored
// or deleted.
var errBoxStopped = errors.New("its box went to sleep, was restored or deleted")

// startJob starts in's command in the background in environment id's box,
// and returns its ID.
func (s *Server) startJob(ctx context.Context, id string, in bashInput) (string, error) {
	s.jobs.mu.Lock()
	running := s.jobs.byEnv[id]
	if len(running) >= maxJobs {
		// Make room by forgetting the oldest finished job.
		i := -1
		for k, j := range running {
			if j.finished() {
				i = k
				break
			}
		}
		if i < 0 {
			s.jobs.mu.Unlock()
			return "", fmt.Errorf("%d background commands are running: stop one with KillShell", maxJobs)
		}
		s.jobs.byEnv[id] = append(running[:i:i], running[i+1:]...)
	}
	s.jobs.next++
	j := &job{id: "bash_" + strconv.Itoa(s.jobs.next), command: in.Command, ended: make(chan struct{})}
	s.jobs.mu.Unlock()

	// The job outlives the call that starts it.
	jctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	j.cancel = cancel
	conn, ex, err := s.dialBox(jctx, id, cancel, s.envs.Job)
	if err != nil {
		cancel()
		return "", err
	}
	s.jobs.mu.Lock()
	if s.jobs.byEnv == nil {
		s.jobs.byEnv = make(map[string][]*job)
	}
	s.jobs.byEnv[id] = append(s.jobs.byEnv[id], j)
	s.jobs.mu.Unlock()
	s.jobs.wg.Go(func() {
		defer close(j.ended)
		defer cancel()
		defer func() { _ = conn.CloseNow() }()
		code, err := boxexec.Run(jctx, conn, boxexec.Request{Argv: []string{"/bin/sh", "-c", in.Command}},
			boxexec.Streams{Stdout: j, Stderr: j})
		if jctx.Err() != nil {
			err = errBoxStopped
		}
		j.mu.Lock()
		j.done, j.code, j.err = true, code, err
		j.mu.Unlock()
		ex.Close()
	})
	return j.id, nil
}

func (j *job) finished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

// findJob returns environment id's job jobID.
func (s *Server) findJob(id, jobID string) (*job, error) {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	for _, j := range s.jobs.byEnv[id] {
		if j.id == jobID {
			return j, nil
		}
	}
	return nil, fmt.Errorf("no background command %s", jobID)
}

// killJob stops environment id's job jobID and waits for it to end.
func (s *Server) killJob(ctx context.Context, id, jobID string) (string, error) {
	j, err := s.findJob(id, jobID)
	if err != nil {
		return "", err
	}
	j.mu.Lock()
	if !j.done {
		j.killed = true
	}
	j.mu.Unlock()
	j.cancel()
	// The cancel ends the command's exec, and fern-init kills its process
	// group.
	select {
	case <-j.ended:
		return j.read(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close stops every background command and waits for them to end.
func (s *Server) Close() {
	s.jobs.mu.Lock()
	for _, js := range s.jobs.byEnv {
		for _, j := range js {
			j.cancel()
		}
	}
	s.jobs.mu.Unlock()
	s.jobs.wg.Wait()
}
