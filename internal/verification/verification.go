// Package verification runs host-defined checks against an exact, clean Git
// commit. It records evidence; sandboxing and persistence are caller concerns.
package verification

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nebler/fern/internal/task"
)

const (
	MaxPolicyTimeout       = time.Hour
	MaxGitTimeout          = time.Minute
	MaxOutputBytes         = 1 << 20
	MaxEnvironmentEntries  = 64
	MaxEnvironmentBytes    = 32 << 10
	maxArgumentEntries     = 256
	maxArgumentBytes       = 32 << 10
	gitDiagnosticByteLimit = 4 << 10
	outputDrainTimeout     = 2 * time.Second
)

var (
	ErrInvalidPolicy  = errors.New("invalid verification policy")
	ErrInvalidRunner  = errors.New("invalid verification runner")
	ErrInvalidRequest = errors.New("invalid verification request")
	ErrPreflight      = errors.New("verification preflight failed")
	ErrExecution      = errors.New("verification command could not be executed")
	ErrIntegrity      = errors.New("verification integrity failure")

	checkNamePattern       = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// PolicyConfig is copied by NewPolicy. Argv[0] must be an absolute executable;
// no shell is inserted. Environment contains exact values, not names to copy
// from the runner's ambient environment.
type PolicyConfig struct {
	CheckName        string
	Argv             []string
	WorkingDirectory string
	Timeout          time.Duration
	Environment      map[string]string
	OutputBytes      int
}

// Policy is immutable outside this package. Its constructor copies all
// reference-backed input.
type Policy struct {
	checkName          string
	argv               []string
	workingDirectory   string
	timeout            time.Duration
	environment        []string
	outputBytes        int
	valid              bool
	sha256             [sha256.Size]byte
	environmentSHA256  [sha256.Size]byte
	executableIdentity executableIdentity
}

// PolicySnapshot is the immutable policy projection persisted with a
// verification. Argv is included so callers can audit the policy digest, but
// command arguments are never accepted from the task store.
type PolicySnapshot struct {
	CheckName         string
	Argv              []string
	WorkingDirectory  string
	Timeout           time.Duration
	OutputBytes       int
	SHA256            [sha256.Size]byte
	ExecutableSHA256  [sha256.Size]byte
	EnvironmentSHA256 [sha256.Size]byte
}

func NewPolicy(config PolicyConfig) (Policy, error) {
	if !checkNamePattern.MatchString(config.CheckName) {
		return Policy{}, fmt.Errorf("%w: check name", ErrInvalidPolicy)
	}
	if len(config.Argv) == 0 || len(config.Argv) > maxArgumentEntries {
		return Policy{}, fmt.Errorf("%w: argv", ErrInvalidPolicy)
	}
	if !filepath.IsAbs(config.Argv[0]) || filepath.Clean(config.Argv[0]) != config.Argv[0] {
		return Policy{}, fmt.Errorf("%w: executable must be an absolute clean path", ErrInvalidPolicy)
	}
	executableIdentity, executableErr := inspectExecutable(config.Argv[0])
	if executableErr != nil {
		return Policy{}, fmt.Errorf("%w: executable is not a trusted host file", ErrInvalidPolicy)
	}
	argumentBytes := 0
	for _, argument := range config.Argv {
		if strings.IndexByte(argument, 0) >= 0 {
			return Policy{}, fmt.Errorf("%w: argv contains NUL", ErrInvalidPolicy)
		}
		argumentBytes += len(argument)
	}
	if argumentBytes > maxArgumentBytes {
		return Policy{}, fmt.Errorf("%w: argv too large", ErrInvalidPolicy)
	}
	workingDirectory, err := validateRelativeDirectory(config.WorkingDirectory)
	if err != nil {
		return Policy{}, err
	}
	if config.Timeout <= 0 || config.Timeout > MaxPolicyTimeout {
		return Policy{}, fmt.Errorf("%w: timeout", ErrInvalidPolicy)
	}
	if config.OutputBytes <= 0 || config.OutputBytes > MaxOutputBytes {
		return Policy{}, fmt.Errorf("%w: output limit", ErrInvalidPolicy)
	}
	environment, err := normalizeEnvironment(config.Environment, nil)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: %v", ErrInvalidPolicy, err)
	}
	canonical := struct {
		Version          string            `json:"version"`
		CheckName        string            `json:"checkName"`
		Argv             []string          `json:"argv"`
		WorkingDirectory string            `json:"workingDirectory"`
		TimeoutMillis    int64             `json:"timeoutMillis"`
		Environment      []string          `json:"environment"`
		OutputBytes      int               `json:"outputBytes"`
		ExecutableSHA256 [sha256.Size]byte `json:"executableSHA256"`
	}{"fern.verification.policy.v2", config.CheckName, config.Argv, workingDirectory,
		config.Timeout.Milliseconds(), environment, config.OutputBytes, executableIdentity.sha256}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: canonical policy", ErrInvalidPolicy)
	}
	return Policy{
		checkName:          config.CheckName,
		argv:               append([]string(nil), config.Argv...),
		workingDirectory:   workingDirectory,
		timeout:            config.Timeout,
		environment:        environment,
		outputBytes:        config.OutputBytes,
		valid:              true,
		sha256:             sha256.Sum256(encoded),
		environmentSHA256:  digestEnvironment(environment),
		executableIdentity: executableIdentity,
	}, nil
}

func (p Policy) CheckName() string        { return p.checkName }
func (p Policy) Argv() []string           { return append([]string(nil), p.argv...) }
func (p Policy) WorkingDirectory() string { return p.workingDirectory }
func (p Policy) Timeout() time.Duration   { return p.timeout }
func (p Policy) OutputBytes() int         { return p.outputBytes }

// SHA256 is the stable digest of the complete canonical policy projection.
func (p Policy) SHA256() [sha256.Size]byte { return p.sha256 }

// EnvironmentSHA256 is the stable digest of the policy environment alone.
func (p Policy) EnvironmentSHA256() [sha256.Size]byte { return p.environmentSHA256 }

// Snapshot returns a detached immutable policy projection.
func (p Policy) Snapshot() PolicySnapshot {
	return PolicySnapshot{CheckName: p.checkName, Argv: append([]string(nil), p.argv...),
		WorkingDirectory: p.workingDirectory, Timeout: p.timeout, OutputBytes: p.outputBytes,
		SHA256: p.sha256, ExecutableSHA256: p.executableIdentity.sha256, EnvironmentSHA256: p.environmentSHA256}
}

// Environment returns a copy of the policy's exact environment additions.
func (p Policy) Environment() []string { return append([]string(nil), p.environment...) }

func (p Policy) validate() error {
	if !p.valid || !checkNamePattern.MatchString(p.checkName) || len(p.argv) == 0 {
		return ErrInvalidPolicy
	}
	return nil
}

// RunnerConfig is trusted host configuration. Environment is an explicit
// minimal environment applied to Git and to checks; it never reads os.Environ.
type RunnerConfig struct {
	GitExecutable string
	GitTimeout    time.Duration
	Environment   map[string]string
	Name          string
	Version       string
	ImageDigest   string
}

// RunnerSnapshot is the immutable host-runner projection persisted by the
// coordinator. EnvironmentSHA256 covers the exact merged runner and policy
// environment used by the check.
type RunnerSnapshot struct {
	Name                string
	Version             string
	ImageDigest         string
	SHA256              [sha256.Size]byte
	GitExecutableSHA256 [sha256.Size]byte
	EnvironmentSHA256   [sha256.Size]byte
}

// Runner contains copied host configuration and is safe for concurrent use.
type Runner struct {
	gitExecutable         string
	gitExecutableIdentity executableIdentity
	gitTimeout            time.Duration
	environment           []string
	name                  string
	version               string
	imageDigest           string
}

func NewRunner(config RunnerConfig) (*Runner, error) {
	if !filepath.IsAbs(config.GitExecutable) || filepath.Clean(config.GitExecutable) != config.GitExecutable {
		return nil, fmt.Errorf("%w: Git executable must be an absolute clean path", ErrInvalidRunner)
	}
	if config.GitTimeout <= 0 || config.GitTimeout > MaxGitTimeout {
		return nil, fmt.Errorf("%w: Git timeout", ErrInvalidRunner)
	}
	gitExecutableIdentity, err := inspectExecutable(config.GitExecutable)
	if err != nil {
		return nil, fmt.Errorf("%w: Git executable", ErrInvalidRunner)
	}
	environment, err := normalizeEnvironment(config.Environment, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRunner, err)
	}
	if !validMetadata(config.Name, 128) || !validMetadata(config.Version, 128) || !validMetadata(config.ImageDigest, 256) {
		return nil, fmt.Errorf("%w: runner metadata", ErrInvalidRunner)
	}
	if err := validateGitExecutable(config.GitExecutable, gitExecutableIdentity, config.GitTimeout, environment); err != nil {
		return nil, fmt.Errorf("%w: pinned Git executable cannot be launched", ErrInvalidRunner)
	}
	return &Runner{
		gitExecutable:         config.GitExecutable,
		gitExecutableIdentity: gitExecutableIdentity,
		gitTimeout:            config.GitTimeout,
		environment:           environment,
		name:                  config.Name,
		version:               config.Version,
		imageDigest:           config.ImageDigest,
	}, nil
}

// Snapshot returns metadata for the exact environment this runner will use
// with policy. It fails on an environment-name collision just as Run does.
func (r *Runner) Snapshot(policy Policy) (RunnerSnapshot, error) {
	if r == nil || r.gitExecutable == "" || r.gitTimeout <= 0 || !validMetadata(r.name, 128) ||
		!validMetadata(r.version, 128) || !validMetadata(r.imageDigest, 256) {
		return RunnerSnapshot{}, ErrInvalidRunner
	}
	if err := policy.validate(); err != nil {
		return RunnerSnapshot{}, err
	}
	environment, err := mergeEnvironment(r.environment, policy.environment)
	if err != nil {
		return RunnerSnapshot{}, fmt.Errorf("%w: environment collision", ErrInvalidPolicy)
	}
	environmentSHA256 := digestEnvironment(environment)
	canonical := struct {
		Version             string            `json:"version"`
		Name                string            `json:"name"`
		RunnerVersion       string            `json:"runnerVersion"`
		ImageDigest         string            `json:"imageDigest"`
		GitExecutable       string            `json:"gitExecutable"`
		GitExecutableSHA256 [sha256.Size]byte `json:"gitExecutableSHA256"`
		EnvironmentSHA256   [sha256.Size]byte `json:"environmentSHA256"`
	}{"fern.verification.runner.v2", r.name, r.version, r.imageDigest, r.gitExecutable,
		r.gitExecutableIdentity.sha256, environmentSHA256}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return RunnerSnapshot{}, ErrInvalidRunner
	}
	return RunnerSnapshot{Name: r.name, Version: r.version, ImageDigest: r.imageDigest,
		SHA256: sha256.Sum256(encoded), GitExecutableSHA256: r.gitExecutableIdentity.sha256,
		EnvironmentSHA256: environmentSHA256}, nil
}

// Request binds a check to repository identity and immutable task/result Git
// identities. RepositoryPath must be an absolute clean path.
type Request struct {
	RepositoryID   task.RepositoryID
	BaseSHA        task.GitOID
	ResultCommit   task.GitOID
	RepositoryPath string
}

func (r Request) validate() error {
	if err := (task.RepositoryTuple{RepositoryID: r.RepositoryID, BaseSHA: r.BaseSHA}).Validate(); err != nil {
		return fmt.Errorf("%w: repository tuple", ErrInvalidRequest)
	}
	if _, err := task.ParseGitOID(string(r.ResultCommit)); err != nil {
		return fmt.Errorf("%w: result commit", ErrInvalidRequest)
	}
	if !filepath.IsAbs(r.RepositoryPath) || filepath.Clean(r.RepositoryPath) != r.RepositoryPath {
		return fmt.Errorf("%w: repository path", ErrInvalidRequest)
	}
	return nil
}

// ValidateResult proves that result carries the same immutable identities as
// the request, in addition to task.ResultTuple's non-Git invariants.
func (r Request) ValidateResult(result task.ResultTuple) error {
	if err := r.validate(); err != nil {
		return err
	}
	expected := task.RepositoryTuple{RepositoryID: r.RepositoryID, BaseSHA: r.BaseSHA}
	if err := result.ValidateAgainst(expected); err != nil {
		return fmt.Errorf("%w: result tuple", ErrInvalidRequest)
	}
	if result.ResultCommit != r.ResultCommit {
		return fmt.Errorf("%w: result commit differs from tuple", ErrInvalidRequest)
	}
	return nil
}

type Failure string

const (
	FailureNone      Failure = ""
	FailurePreflight Failure = "preflight"
	FailureStart     Failure = "start"
	FailureCommand   Failure = "command"
	FailureTimeout   Failure = "timeout"
	FailureCancelled Failure = "cancelled"
	FailureIntegrity Failure = "integrity"
)

// OutputEvidence contains a bounded prefix but accounts for and hashes every
// byte drained from one output stream.
type OutputEvidence struct {
	ByteCount int64
	SHA256    [sha256.Size]byte
	Prefix    []byte
	Truncated bool
}

// Result is the complete in-memory execution record. A non-nil error never
// contains command or Git output.
type Result struct {
	CheckName      string
	RepositoryID   task.RepositoryID
	BaseSHA        task.GitOID
	ResultCommit   task.GitOID
	StartedAt      time.Time
	EndedAt        time.Time
	Executed       bool
	ExitCode       int
	Signal         string
	TimedOut       bool
	Cancelled      bool
	Stdout         OutputEvidence
	Stderr         OutputEvidence
	Failure        Failure
	IntegrityError bool
	Success        bool
}

var errPolicyTimeout = errors.New("verification policy timeout")

// Run executes a policy once. It never retries and never cleans or reverts the
// repository. Command exit failures, timeouts, and cancellation are represented
// in Result; integrity and runner failures also return a redacted error.
func (r *Runner) Run(ctx context.Context, policy Policy, request Request) (Result, error) {
	result := Result{
		CheckName:    policy.checkName,
		RepositoryID: request.RepositoryID,
		BaseSHA:      request.BaseSHA,
		ResultCommit: request.ResultCommit,
		ExitCode:     -1,
	}
	if r == nil || r.gitExecutable == "" || r.gitTimeout <= 0 {
		return result, ErrInvalidRunner
	}
	if err := policy.validate(); err != nil {
		return result, err
	}
	if err := request.validate(); err != nil {
		return result, err
	}

	repositoryPath, err := secureRepositoryPath(request.RepositoryPath)
	if err != nil {
		return result, err
	}
	workingDirectory, err := secureWorkingDirectory(repositoryPath, policy.workingDirectory)
	if err != nil {
		return result, err
	}
	if err := r.checkRepository(ctx, repositoryPath, request); err != nil {
		result.Failure = FailurePreflight
		return result, ErrPreflight
	}
	repositoryIdentity, repositoryErr := os.Stat(repositoryPath)
	workingIdentity, workingErr := os.Stat(workingDirectory)
	gitIdentity, gitErr := os.Stat(filepath.Join(repositoryPath, ".git"))
	if repositoryErr != nil || workingErr != nil || gitErr != nil {
		result.Failure = FailurePreflight
		return result, ErrPreflight
	}

	environment, err := mergeEnvironment(r.environment, policy.environment)
	if err != nil {
		return result, fmt.Errorf("%w: environment collision", ErrInvalidPolicy)
	}
	stdout := newEvidenceWriter(policy.outputBytes)
	stderr := newEvidenceWriter(policy.outputBytes)
	executable, commandErr := preparePinnedExecutable(policy.argv[0], policy.executableIdentity)
	if commandErr != nil {
		result.Failure = FailureStart
		return result, ErrExecution
	}
	defer executable.close()
	runContext, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	command := exec.CommandContext(runContext, executable.path, policy.argv[1:]...)
	command.Args[0] = policy.argv[0]
	command.ExtraFiles = executable.extraFiles
	command.Dir = workingDirectory
	command.Env = environment
	result.StartedAt = time.Now().UTC()
	var timeout *time.Timer
	waitErr, cleanupErr, executed := runContainedCommand(command, stdout, stderr, func() {
		timeout = time.AfterFunc(policy.timeout, func() { cancel(errPolicyTimeout) })
	})
	if timeout != nil {
		timeout.Stop()
	}
	result.Executed = executed
	result.EndedAt = time.Now().UTC()
	result.Stdout = stdout.evidence()
	result.Stderr = stderr.evidence()
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
		result.Signal = processSignal(command.ProcessState)
	}

	cause := context.Cause(runContext)
	result.TimedOut = errors.Is(cause, errPolicyTimeout)
	result.Cancelled = cause != nil && !result.TimedOut
	if !result.Executed {
		result.Failure = FailureStart
		return result, ErrExecution
	}
	if cleanupErr != nil {
		result.Failure = FailureIntegrity
		result.IntegrityError = true
		return result, ErrIntegrity
	}

	// Postflight takes precedence over every command outcome. Once execution
	// occurred, inability to prove the exact clean commit is an integrity error.
	postRepository, repositoryErr := secureRepositoryPath(request.RepositoryPath)
	postWorking, workingErr := secureWorkingDirectory(repositoryPath, policy.workingDirectory)
	postRepositoryIdentity, repositoryStatErr := os.Stat(postRepository)
	postWorkingIdentity, workingStatErr := os.Stat(postWorking)
	postGitIdentity, gitStatErr := os.Stat(filepath.Join(postRepository, ".git"))
	pathsChanged := repositoryErr != nil || workingErr != nil || repositoryStatErr != nil || workingStatErr != nil || gitStatErr != nil ||
		postRepository != repositoryPath || postWorking != workingDirectory ||
		!os.SameFile(repositoryIdentity, postRepositoryIdentity) || !os.SameFile(workingIdentity, postWorkingIdentity) ||
		!os.SameFile(gitIdentity, postGitIdentity)
	if pathsChanged || r.checkRepository(context.WithoutCancel(ctx), repositoryPath, request) != nil {
		result.Failure = FailureIntegrity
		result.IntegrityError = true
		return result, ErrIntegrity
	}

	switch {
	case result.TimedOut:
		result.Failure = FailureTimeout
	case result.Cancelled:
		result.Failure = FailureCancelled
	case result.Signal != "" || result.ExitCode != 0 || waitErr != nil:
		result.Failure = FailureCommand
	default:
		result.Success = true
	}
	return result, nil
}

type evidenceWriter struct {
	hash      hash.Hash
	prefix    []byte
	limit     int
	byteCount int64
}

func newEvidenceWriter(limit int) *evidenceWriter {
	return &evidenceWriter{hash: sha256.New(), limit: limit}
}

func (w *evidenceWriter) Write(p []byte) (int, error) {
	n, err := w.hash.Write(p)
	w.byteCount += int64(n)
	remaining := w.limit - len(w.prefix)
	if remaining > n {
		remaining = n
	}
	if remaining > 0 {
		w.prefix = append(w.prefix, p[:remaining]...)
	}
	return n, err
}

func (w *evidenceWriter) evidence() OutputEvidence {
	h := w.hash.Sum(nil)
	var sum [sha256.Size]byte
	copy(sum[:], h)
	return OutputEvidence{
		ByteCount: w.byteCount,
		SHA256:    sum,
		Prefix:    append([]byte(nil), w.prefix...),
		Truncated: w.byteCount > int64(w.limit),
	}
}

func validateRelativeDirectory(directory string) (string, error) {
	if directory == "" || directory == "." {
		return "", nil
	}
	if filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", fmt.Errorf("%w: working directory must be a clean repository-relative path", ErrInvalidPolicy)
	}
	if directory == ".." || strings.HasPrefix(directory, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: working directory escapes repository", ErrInvalidPolicy)
	}
	return directory, nil
}

func normalizeEnvironment(environment map[string]string, forbidden map[string]struct{}) ([]string, error) {
	if len(environment) > MaxEnvironmentEntries {
		return nil, errors.New("too many environment entries")
	}
	names := make([]string, 0, len(environment))
	total := 0
	for name, value := range environment {
		if !environmentNamePattern.MatchString(name) || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("invalid environment entry")
		}
		if _, found := forbidden[name]; found {
			return nil, errors.New("environment entry collision")
		}
		total += len(name) + len(value) + 1
		names = append(names, name)
	}
	if total > MaxEnvironmentBytes {
		return nil, errors.New("environment too large")
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result, nil
}

func mergeEnvironment(base, additions []string) ([]string, error) {
	if len(base)+len(additions) > MaxEnvironmentEntries {
		return nil, errors.New("too many environment entries")
	}
	result := make([]string, len(base), len(base)+len(additions))
	copy(result, base)
	names := make(map[string]struct{}, len(base))
	total := 0
	for _, entry := range base {
		names[strings.SplitN(entry, "=", 2)[0]] = struct{}{}
		total += len(entry)
	}
	for _, entry := range additions {
		name := strings.SplitN(entry, "=", 2)[0]
		if _, found := names[name]; found {
			return nil, errors.New("duplicate environment name")
		}
		names[name] = struct{}{}
		result = append(result, entry)
		total += len(entry)
	}
	if total > MaxEnvironmentBytes {
		return nil, errors.New("environment too large")
	}
	sort.Strings(result)
	return result, nil
}

func digestEnvironment(environment []string) [sha256.Size]byte {
	encoded, _ := json.Marshal(struct {
		Version     string   `json:"version"`
		Environment []string `json:"environment"`
	}{"fern.verification.environment.v1", environment})
	return sha256.Sum256(encoded)
}

func validMetadata(value string, limit int) bool {
	return len(value) > 0 && len(value) <= limit && !strings.ContainsAny(value, "\x00\r\n")
}

func secureRepositoryPath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: repository path is not a real directory", ErrInvalidRequest)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: repository path", ErrInvalidRequest)
	}
	if realPath != path {
		return "", fmt.Errorf("%w: repository path contains a symlink", ErrInvalidRequest)
	}
	gitDirectory := filepath.Join(path, ".git")
	gitInfo, err := os.Lstat(gitDirectory)
	if err != nil || !gitInfo.IsDir() || gitInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: repository must have a real Git directory", ErrInvalidRequest)
	}
	realGitDirectory, err := filepath.EvalSymlinks(gitDirectory)
	if err != nil || realGitDirectory != gitDirectory {
		return "", fmt.Errorf("%w: Git directory contains a symlink", ErrInvalidRequest)
	}
	return path, nil
}

func secureWorkingDirectory(repositoryPath, relative string) (string, error) {
	if relative == "" {
		return repositoryPath, nil
	}
	candidate := filepath.Join(repositoryPath, relative)
	if !pathWithin(repositoryPath, candidate) {
		return "", fmt.Errorf("%w: working directory escapes repository", ErrInvalidPolicy)
	}
	current := repositoryPath
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: working directory contains a symlink", ErrInvalidRequest)
		}
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: working directory is not a directory", ErrInvalidRequest)
	}
	return candidate, nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (r *Runner) checkRepository(ctx context.Context, repositoryPath string, request Request) error {
	topLevel, ok := r.runGit(ctx, repositoryPath, "rev-parse", "--show-toplevel")
	if !ok || topLevel.truncated || topLevel.byteCount != int64(len(topLevel.prefix)) || !strings.HasSuffix(string(topLevel.prefix), "\n") {
		return errors.New("repository top-level not proven")
	}
	reportedTopLevel := strings.TrimSuffix(string(topLevel.prefix), "\n")
	if filepath.Clean(filepath.FromSlash(reportedTopLevel)) != repositoryPath {
		return errors.New("repository top-level differs")
	}
	checks := []struct {
		arguments []string
		expected  string
		empty     bool
	}{
		{[]string{"rev-parse", "--is-inside-work-tree"}, "true\n", false},
		{[]string{"rev-parse", "--show-object-format"}, "sha1\n", false},
		{[]string{"cat-file", "-t", string(request.BaseSHA)}, "commit\n", false},
		{[]string{"cat-file", "-t", string(request.ResultCommit)}, "commit\n", false},
		{[]string{"rev-parse", "--verify", "HEAD"}, string(request.ResultCommit) + "\n", false},
		{[]string{"status", "--porcelain=v1", "--untracked-files=all"}, "", true},
	}
	for _, check := range checks {
		stdout, ok := r.runGit(ctx, repositoryPath, check.arguments...)
		if !ok || (check.empty && stdout.byteCount != 0) || (!check.empty && string(stdout.prefix) != check.expected) || stdout.truncated {
			return errors.New("repository state not proven")
		}
	}
	return nil
}

type gitOutput struct {
	byteCount int64
	prefix    []byte
	truncated bool
}

func (r *Runner) runGit(ctx context.Context, repositoryPath string, arguments ...string) (gitOutput, bool) {
	gitContext, cancel := context.WithTimeout(ctx, r.gitTimeout)
	defer cancel()
	args := []string{"--no-pager", "--no-replace-objects", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-C", repositoryPath}
	args = append(args, arguments...)
	stdout := newEvidenceWriter(gitDiagnosticByteLimit)
	stderr := newEvidenceWriter(gitDiagnosticByteLimit)
	command, releaseExecutable, commandErr := gitCommand(gitContext, r.gitExecutable, r.gitExecutableIdentity, args...)
	if commandErr != nil {
		return gitOutput{}, false
	}
	defer releaseExecutable()
	command.Env = make([]string, len(r.environment))
	copy(command.Env, r.environment)
	err, cleanupErr, executed := runContainedCommand(command, stdout, stderr, nil)
	evidence := stdout.evidence()
	return gitOutput{byteCount: evidence.ByteCount, prefix: evidence.Prefix, truncated: evidence.Truncated},
		executed && err == nil && cleanupErr == nil && gitContext.Err() == nil
}

// runContainedCommand uses caller-owned pipes so Cmd.Wait returns as soon as
// the group leader exits. It can then kill the entire group before waiting for
// inherited output descriptors to close, including when the leader succeeded.
func runContainedCommand(command *exec.Cmd, stdout, stderr io.Writer, afterStart func()) (waitErr, cleanupErr error, executed bool) {
	if err := configureProcessGroup(command); err != nil {
		return err, nil, false
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return err, nil, false
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return err, nil, false
	}
	command.Stdout = stdoutWrite
	command.Stderr = stderrWrite
	if err := command.Start(); err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		_ = stderrRead.Close()
		_ = stderrWrite.Close()
		return err, nil, false
	}
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	if afterStart != nil {
		afterStart()
	}
	drainErrors := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(stdout, stdoutRead)
		closeErr := stdoutRead.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		drainErrors <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(stderr, stderrRead)
		closeErr := stderrRead.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		drainErrors <- copyErr
	}()
	waitErr = command.Wait()
	cleanupErr = teardownProcessGroup(command)
	drainTimer := time.NewTimer(outputDrainTimeout)
	defer drainTimer.Stop()
	for completed := 0; completed < 2; {
		select {
		case err := <-drainErrors:
			completed++
			if err != nil && cleanupErr == nil {
				cleanupErr = err
			}
		case <-drainTimer.C:
			if cleanupErr == nil {
				cleanupErr = errors.New("command output drain did not terminate")
			}
			_ = stdoutRead.Close()
			_ = stderrRead.Close()
			for completed < 2 {
				<-drainErrors
				completed++
			}
		}
	}
	return waitErr, cleanupErr, true
}
