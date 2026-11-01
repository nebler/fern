package boxexec

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"slices"
	"sync"
	"syscall"
)

// JobsPath is where fern-init keeps the box's background commands (Jobs):
// POST JobsPath starts one, GET lists them, and JobsPath/{id}/read, /kill
// (POST) and /wait (GET) read, stop and wait for one.
const JobsPath = "/jobs"

// Limits of a box's background commands: how many it keeps, finished ones
// included, and how much unread output one keeps, its newest.
const (
	MaxJobs      = 16
	maxJobOutput = 1 << 20
)

// JobRequest starts a background command.
type JobRequest struct {
	ID      string   `json:"id"`
	Argv    []string `json:"argv"`
	Environ []string `json:"environ,omitempty"`
}

// JobInfo is a background command as GET JobsPath lists it.
type JobInfo struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
}

// JobOutput is what a read or a kill returns: the output since the last
// read, and how the command is.
type JobOutput struct {
	Output  string `json:"output"`
	Dropped int    `json:"dropped,omitempty"` // bytes of output dropped, the oldest, past what a job keeps
	Running bool   `json:"running"`
	Killed  bool   `json:"killed,omitempty"`
	Code    int    `json:"code"`
}

// Jobs are a box's background commands, such as dev servers Claude starts.
// Unlike an exec, a job isn't tied to a connection: it runs on while the
// daemon restarts, which finds it again (GET JobsPath), and ends when it
// exits, is killed, or the box stops (Close).
type Jobs struct {
	token string
	dir   string
	env   []string
	log   *slog.Logger

	mu   sync.Mutex
	jobs []*job // oldest first
	wg   sync.WaitGroup
}

// job is one background command. Its fields below mu change as it runs.
type job struct {
	id    string
	cmd   *exec.Cmd
	ended chan struct{} // closed once it has exited

	mu      sync.Mutex
	out     []byte
	dropped int
	killed  bool
	code    int
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

// NewJobs returns the Jobs of a box whose commands run in dir with
// environment env, for clients that present token.
func NewJobs(token, dir string, env []string, log *slog.Logger) *Jobs {
	return &Jobs{token: token, dir: dir, env: env, log: log}
}

// Register adds the jobs' routes to mux.
func (js *Jobs) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+JobsPath, js.auth(js.start))
	mux.HandleFunc("GET "+JobsPath, js.auth(js.list))
	mux.HandleFunc("POST "+JobsPath+"/{id}/read", js.auth(js.read))
	mux.HandleFunc("POST "+JobsPath+"/{id}/kill", js.auth(js.kill))
	mux.HandleFunc("GET "+JobsPath+"/{id}/wait", js.auth(js.wait))
}

func (js *Jobs) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+js.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (js *Jobs) start(w http.ResponseWriter, r *http.Request) {
	var req JobRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxMessage)).Decode(&req); err != nil || req.ID == "" || len(req.Argv) == 0 {
		http.Error(w, "a job needs an id and a command", http.StatusBadRequest)
		return
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	if slices.ContainsFunc(js.jobs, func(j *job) bool { return j.id == req.ID }) {
		http.Error(w, "job "+req.ID+" exists", http.StatusConflict)
		return
	}
	if len(js.jobs) >= MaxJobs {
		// Make room by forgetting the oldest finished job.
		i := slices.IndexFunc(js.jobs, func(j *job) bool { return isDone(j.ended) })
		if i < 0 {
			http.Error(w, fmt.Sprintf("%d background commands are running: stop one", MaxJobs), http.StatusTooManyRequests)
			return
		}
		js.jobs = slices.Delete(js.jobs, i, i+1)
	}
	env := append(slices.Clone(js.env), req.Environ...)
	// The job outlives the request, and the daemon: kill and Close end it.
	cmd := exec.CommandContext(context.WithoutCancel(r.Context()), lookPath(req.Argv[0], env), req.Argv[1:]...)
	cmd.Dir, cmd.Env = js.dir, env
	// Its own process group, killed whole, as an exec's.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	j := &job{id: req.ID, cmd: cmd, ended: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = j, j
	// A process the command left behind may hold its output open.
	cmd.WaitDelay = outputDelay
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	js.jobs = append(js.jobs, j)
	js.wg.Go(func() {
		err := cmd.Wait()
		// What the command left in its group ends with it, as with an exec.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		j.mu.Lock()
		j.code = exitCode(err, js.log)
		j.mu.Unlock()
		close(j.ended)
	})
	w.WriteHeader(http.StatusNoContent)
}

func (js *Jobs) list(w http.ResponseWriter, _ *http.Request) {
	js.mu.Lock()
	infos := make([]JobInfo, 0, len(js.jobs))
	for _, j := range js.jobs {
		infos = append(infos, JobInfo{ID: j.id, Running: !isDone(j.ended)})
	}
	js.mu.Unlock()
	writeJSONTo(w, infos)
}

func (js *Jobs) find(w http.ResponseWriter, r *http.Request) *job {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.jobs {
		if j.id == r.PathValue("id") {
			return j
		}
	}
	http.Error(w, "no job "+r.PathValue("id"), http.StatusNotFound)
	return nil
}

// read returns the output since the last read.
func (js *Jobs) read(w http.ResponseWriter, r *http.Request) {
	if j := js.find(w, r); j != nil {
		writeJSONTo(w, j.take())
	}
}

// kill stops the job's process group, waits for it, and returns its output.
func (js *Jobs) kill(w http.ResponseWriter, r *http.Request) {
	j := js.find(w, r)
	if j == nil {
		return
	}
	if !isDone(j.ended) {
		j.mu.Lock()
		j.killed = true
		j.mu.Unlock()
		_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL)
		<-j.ended
	}
	writeJSONTo(w, j.take())
}

// wait answers once the job has ended, or the client has gone.
func (js *Jobs) wait(w http.ResponseWriter, r *http.Request) {
	j := js.find(w, r)
	if j == nil {
		return
	}
	select {
	case <-j.ended:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

// take returns the unread output and the job's state, and forgets the
// output.
func (j *job) take() JobOutput {
	done := isDone(j.ended)
	j.mu.Lock()
	defer j.mu.Unlock()
	o := JobOutput{Output: string(j.out), Dropped: j.dropped, Running: !done, Killed: j.killed && done, Code: j.code}
	j.out, j.dropped = nil, 0
	return o
}

// Close kills every job and waits for them: the box is stopping.
func (js *Jobs) Close() {
	js.mu.Lock()
	for _, j := range js.jobs {
		if !isDone(j.ended) {
			_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	js.mu.Unlock()
	js.wg.Wait()
}

func isDone(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func writeJSONTo(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// JobClient is the daemon's side of a box's Jobs, at address addr.
type JobClient struct {
	HC    *http.Client
	Addr  string // host:port of the box's fern-init
	Token string
}

// ErrNoJob means the box doesn't have the job: it was forgotten, or the box
// is a new one.
var ErrNoJob = errors.New("no such background command in the box")

// Start starts a job.
func (c JobClient) Start(ctx context.Context, req JobRequest) error {
	return c.call(ctx, http.MethodPost, JobsPath, req, nil)
}

// List returns the box's jobs.
func (c JobClient) List(ctx context.Context) ([]JobInfo, error) {
	var infos []JobInfo
	return infos, c.call(ctx, http.MethodGet, JobsPath, nil, &infos)
}

// Read returns job id's output since the last read.
func (c JobClient) Read(ctx context.Context, id string) (JobOutput, error) {
	var o JobOutput
	return o, c.call(ctx, http.MethodPost, JobsPath+"/"+id+"/read", nil, &o)
}

// Kill stops job id and returns its last output.
func (c JobClient) Kill(ctx context.Context, id string) (JobOutput, error) {
	var o JobOutput
	return o, c.call(ctx, http.MethodPost, JobsPath+"/"+id+"/kill", nil, &o)
}

// Wait returns once job id has ended; an error if it can't tell, as when
// the box has stopped.
func (c JobClient) Wait(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodGet, JobsPath+"/"+id+"/wait", nil, nil)
}

func (c JobClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.Addr+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoJob
	case resp.StatusCode/100 != 2:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	case out == nil:
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
