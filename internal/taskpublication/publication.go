package taskpublication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/task"
)

const (
	defaultTimeout     = 2 * time.Minute
	defaultOutputLimit = 64 << 10
	maxPullTitleBytes  = 256
	maxPullBodyBytes   = 60 << 10
)

var (
	ErrInvalidConfiguration = errors.New("invalid task publication configuration")
	ErrInvalidRequest       = errors.New("invalid task publication request")
	ErrRepositoryConflict   = errors.New("publication repository identity conflicts")
	ErrBaseMoved            = errors.New("publication base branch moved")
	ErrBranchConflict       = errors.New("publication branch conflicts")
	ErrPushFailed           = errors.New("publication push failed")
	ErrPullRequestUncertain = errors.New("pull request mutation could not be reconciled")
	ErrPullRequestConflict  = errors.New("pull request conflicts with publication")
	ErrGitTimeout           = errors.New("publication Git command timed out")
	ErrGitFailed            = errors.New("publication Git command failed")
)

// Request contains immutable coordinator-owned values. Publisher validates the
// complete tuple before reading GitHub or starting Git.
type Request struct {
	WorkspaceRepository task.RepositoryID
	Task                task.RepositoryTuple
	Result              task.ResultTuple
	Verification        task.VerificationTuple
	Publication         task.PublicationTuple
	Title               string
	Body                string
}

// OutputEvidence proves the amount and digest of subprocess output without
// retaining or returning any output bytes.
type OutputEvidence struct {
	Bytes       int64
	HashedBytes int64
	SHA256      [32]byte
	Truncated   bool
}

// GitEvidence is sanitized process evidence for the one push attempt. It never
// contains argv, environment, paths, credentials, or subprocess output.
type GitEvidence struct {
	Attempted bool
	ExitCode  int
	TimedOut  bool
	Stdout    OutputEvidence
	Stderr    OutputEvidence
}

// Proof is the exact remotely observed result tuple and bounded mutation
// evidence. Already-reconciled publications have Attempted false.
type Proof struct {
	Observation                task.PublicationObservation
	Push                       GitEvidence
	PullRequestCreateConfirmed bool
}

// BranchObservation is an authoritative read of the exact publication ref.
type BranchObservation struct {
	SHA    task.GitOID
	Exists bool
}

// BranchProof contains the authoritative read made after one push invocation.
type BranchProof struct {
	Observation BranchObservation
	Push        GitEvidence
}

// PullRequestProof contains exact PR discovery and exact-number observation.
// Found false is authoritative absence. CreateAttempted records that the POST
// was invoked; CreateConfirmed reports only whether its response was received.
type PullRequestProof struct {
	Observation     task.PublicationObservation
	Found           bool
	CreateAttempted bool
	CreateConfirmed bool
}

// Config contains only host-owned transport settings. Repository URLs,
// refspecs, credentials, and force options are derived internally.
type Config struct {
	RepositoryPath string
	GitExecutable  string
	TempRoot       string
	Timeout        time.Duration
	OutputLimit    int64
	Now            func() time.Time
}

type Publisher struct {
	repositoryPath string
	gitExecutable  string
	tempRoot       string
	timeout        time.Duration
	outputLimit    int64
	now            func() time.Time
	tokens         githubapp.InstallationTokenSource
	repositories   *githubapp.RepositoryClient
}

func New(config Config, tokens githubapp.InstallationTokenSource, repositories *githubapp.RepositoryClient) (*Publisher, error) {
	if tokens == nil || repositories == nil || config.RepositoryPath == "" || config.GitExecutable == "" || !filepath.IsAbs(config.RepositoryPath) || !filepath.IsAbs(config.GitExecutable) {
		return nil, ErrInvalidConfiguration
	}
	repositoryPath, err := secureDirectory(config.RepositoryPath, false)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	executablePath := filepath.Clean(config.GitExecutable)
	resolvedExecutable, executableResolveErr := filepath.EvalSymlinks(executablePath)
	info, err := os.Lstat(repositoryPath)
	gitInfo, gitErr := os.Lstat(filepath.Join(repositoryPath, ".git"))
	executableInfo, executableErr := os.Lstat(executablePath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 ||
		gitErr != nil || !gitInfo.IsDir() || gitInfo.Mode()&os.ModeSymlink != 0 || gitInfo.Mode().Perm()&0o022 != 0 ||
		executableResolveErr != nil || resolvedExecutable != executablePath ||
		executableErr != nil || !executableInfo.Mode().IsRegular() || executableInfo.Mode()&os.ModeSymlink != 0 ||
		executableInfo.Mode()&0o111 == 0 || executableInfo.Mode().Perm()&0o022 != 0 {
		return nil, ErrInvalidConfiguration
	}
	if config.TempRoot == "" {
		config.TempRoot = os.TempDir()
	}
	if !filepath.IsAbs(config.TempRoot) {
		return nil, ErrInvalidConfiguration
	}
	config.TempRoot, err = secureDirectory(config.TempRoot, true)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	if config.Timeout == 0 {
		config.Timeout = defaultTimeout
	}
	if config.OutputLimit == 0 {
		config.OutputLimit = defaultOutputLimit
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Timeout <= 0 || config.OutputLimit <= 0 || config.OutputLimit > 1<<20 {
		return nil, ErrInvalidConfiguration
	}
	return &Publisher{
		repositoryPath: repositoryPath, gitExecutable: executablePath,
		tempRoot: config.TempRoot, timeout: config.Timeout, outputLimit: config.OutputLimit,
		now: config.Now, tokens: tokens, repositories: repositories,
	}, nil
}

func secureDirectory(path string, private bool) (string, error) {
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil || resolved != clean {
		return "", ErrInvalidConfiguration
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return "", ErrInvalidConfiguration
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return "", ErrInvalidConfiguration
	}
	return clean, nil
}

// ReconcileBranch performs only authoritative reads of repository identity,
// the pinned base, and the exact publication branch.
func (publisher *Publisher) ReconcileBranch(ctx context.Context, request Request) (BranchObservation, error) {
	operationContext, cancel, identity, err := publisher.begin(ctx, request)
	if cancel != nil {
		defer cancel()
	}
	if err != nil {
		return BranchObservation{}, err
	}
	sha, exists, err := publisher.readBranch(operationContext, identity, request.Publication)
	if err != nil {
		return BranchObservation{}, err
	}
	return BranchObservation{SHA: sha, Exists: exists}, nil
}

// PushOnce invokes Git push at most once and then reads the exact branch. It
// never retries a failed or ambiguously answered mutation.
func (publisher *Publisher) PushOnce(ctx context.Context, request Request) (BranchProof, error) {
	var proof BranchProof
	operationContext, cancel, identity, err := publisher.begin(ctx, request)
	if cancel != nil {
		defer cancel()
	}
	if err != nil {
		return proof, err
	}
	if err := publisher.validateLocalGitConfig(operationContext); err != nil {
		return proof, err
	}
	if err := publisher.proveLocalCommit(operationContext, request.Publication.ResultCommit); err != nil {
		return proof, err
	}
	proof.Push, err = publisher.push(operationContext, identity, request.Publication)
	// The follow-up read reconciles possibly lost pushes, but a timeout has
	// already exhausted the operation budget: the read context is expired by
	// construction. Report the underlying push error, never the read's.
	sha, exists, readErr := publisher.readBranch(operationContext, identity, request.Publication)
	proof.Observation = BranchObservation{SHA: sha, Exists: exists}
	if readErr != nil {
		if err != nil {
			return proof, err
		}
		return proof, readErr
	}
	if exists && sha == request.Publication.ResultCommit {
		return proof, nil
	}
	if exists {
		return proof, ErrBranchConflict
	}
	if err != nil {
		return proof, err
	}
	return proof, ErrPushFailed
}

// ReconcilePullRequest performs exact branch and PR reads without mutation.
func (publisher *Publisher) ReconcilePullRequest(ctx context.Context, request Request) (PullRequestProof, error) {
	operationContext, cancel, identity, err := publisher.begin(ctx, request)
	if cancel != nil {
		defer cancel()
	}
	if err != nil {
		return PullRequestProof{}, err
	}
	return publisher.readPullRequest(operationContext, identity, request.Publication)
}

// CreatePullRequestOnce performs at most one create, then discovers the exact
// PR identity and re-reads that exact number. The mutation is never retried.
func (publisher *Publisher) CreatePullRequestOnce(ctx context.Context, request Request) (PullRequestProof, error) {
	operationContext, cancel, identity, err := publisher.begin(ctx, request)
	if cancel != nil {
		defer cancel()
	}
	if err != nil {
		return PullRequestProof{}, err
	}
	remoteSHA, exists, err := publisher.readBranch(operationContext, identity, request.Publication)
	if err != nil {
		return PullRequestProof{}, err
	}
	if !exists || remoteSHA != request.Publication.ResultCommit {
		return PullRequestProof{}, ErrBranchConflict
	}
	proof := PullRequestProof{CreateAttempted: true}
	created, createErr := publisher.repositories.CreateDraftPullRequest(operationContext, identity, request.Publication.RepositoryFullName,
		request.Publication.BaseRef, request.Publication.Branch, request.Title, request.Body)
	observed, readErr := publisher.readPullRequest(operationContext, identity, request.Publication)
	proof.Observation, proof.Found = observed.Observation, observed.Found
	proof.CreateConfirmed = createErr == nil
	if readErr != nil {
		return proof, readErr
	}
	if !proof.Found {
		return proof, ErrPullRequestUncertain
	}
	if createErr == nil && task.PullRequestNumber(created) != proof.Observation.PullRequest.Number {
		return proof, ErrPullRequestConflict
	}
	return proof, nil
}

func (publisher *Publisher) begin(ctx context.Context, request Request) (context.Context, context.CancelFunc, githubapp.RepositoryIdentity, error) {
	if publisher == nil || publisher.tokens == nil || publisher.repositories == nil || publisher.now == nil || ctx == nil {
		return nil, nil, githubapp.RepositoryIdentity{}, ErrInvalidConfiguration
	}
	if err := validateRequest(request); err != nil {
		return nil, nil, githubapp.RepositoryIdentity{}, err
	}
	operationContext, cancel := context.WithTimeout(ctx, publisher.timeout)
	identity, err := githubapp.NewRepositoryIdentity(int64(request.Publication.InstallationID), int64(request.Publication.RepositoryID))
	if err != nil {
		cancel()
		return nil, nil, githubapp.RepositoryIdentity{}, ErrInvalidRequest
	}
	repository, err := publisher.repositories.RepositoryByID(operationContext, identity, request.Publication.RepositoryFullName)
	if err != nil {
		cancel()
		return nil, nil, githubapp.RepositoryIdentity{}, err
	}
	if repository.RepositoryID() != int64(request.Publication.RepositoryID) || repository.FullName() != request.Publication.RepositoryFullName {
		cancel()
		return nil, nil, githubapp.RepositoryIdentity{}, ErrRepositoryConflict
	}
	base, err := publisher.repositories.BranchReference(operationContext, identity, request.Publication.RepositoryFullName, request.Publication.BaseRef)
	if err != nil {
		cancel()
		return nil, nil, githubapp.RepositoryIdentity{}, err
	}
	if base.SHA() != string(request.Publication.BaseSHA) {
		cancel()
		return nil, nil, githubapp.RepositoryIdentity{}, ErrBaseMoved
	}
	return operationContext, cancel, identity, nil
}

// PublishOrReconcile performs at most one push and one pull request creation.
// Every possibly lost mutation response is followed by an exact authoritative
// read; mutation calls are never retried.
func (publisher *Publisher) PublishOrReconcile(ctx context.Context, request Request) (Proof, error) {
	var proof Proof
	if publisher == nil || publisher.tokens == nil || publisher.repositories == nil || publisher.now == nil || ctx == nil {
		return proof, ErrInvalidConfiguration
	}
	if err := validateRequest(request); err != nil {
		return proof, err
	}
	validationContext, validationCancel := context.WithTimeout(ctx, publisher.timeout)
	defer validationCancel()
	if err := publisher.validateLocalGitConfig(validationContext); err != nil {
		return proof, err
	}
	branch, err := publisher.ReconcileBranch(ctx, request)
	if err != nil {
		return proof, err
	}
	needPush, err := reconcileBranch(request.Publication, branch.SHA, branch.Exists)
	if err != nil {
		return proof, err
	}
	if needPush {
		pushed, pushErr := publisher.PushOnce(ctx, request)
		proof.Push = pushed.Push
		branch = pushed.Observation
		if pushErr != nil {
			return proof, pushErr
		}
	}
	pull, err := publisher.ReconcilePullRequest(ctx, request)
	if err != nil {
		return proof, err
	}
	if !pull.Found {
		pull, err = publisher.CreatePullRequestOnce(ctx, request)
		proof.PullRequestCreateConfirmed = pull.CreateConfirmed
		if err != nil {
			return proof, err
		}
	}
	proof.Observation = pull.Observation
	return proof, nil
}

func (publisher *Publisher) validateLocalGitConfig(ctx context.Context) error {
	commandContext, cancel := context.WithTimeout(ctx, publisher.timeout)
	defer cancel()
	var names bytes.Buffer
	namesWriter := &boundedBuffer{buffer: &names, remaining: 64 << 10}
	stderr := newDigestWriter(publisher.outputLimit)
	command := exec.CommandContext(commandContext, publisher.gitExecutable,
		"--no-pager", "--no-replace-objects", "-C", publisher.repositoryPath,
		"config", "--local", "--no-includes", "--name-only", "--null", "--list")
	command.Dir = publisher.repositoryPath
	command.Env = []string{
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"HOME=" + publisher.tempRoot, "LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin", "XDG_CONFIG_HOME=" + publisher.tempRoot,
	}
	command.Stdout = namesWriter
	command.Stderr = stderr
	configureProcessGroup(command)
	if err := command.Run(); err != nil || commandContext.Err() != nil {
		if commandContext.Err() != nil {
			return ErrGitTimeout
		}
		return ErrGitFailed
	}
	if namesWriter.overflow {
		return ErrInvalidRequest
	}
	for _, rawName := range bytes.Split(names.Bytes(), []byte{0}) {
		if len(rawName) == 0 {
			continue
		}
		name := strings.ToLower(string(rawName))
		if unsafeGitConfigName(name) {
			return ErrInvalidRequest
		}
	}
	return nil
}

type boundedBuffer struct {
	buffer    *bytes.Buffer
	remaining int
	overflow  bool
}

func (writer *boundedBuffer) Write(value []byte) (int, error) {
	n := len(value)
	if len(value) > writer.remaining {
		value = value[:writer.remaining]
		writer.overflow = true
	}
	_, _ = writer.buffer.Write(value)
	writer.remaining -= len(value)
	return n, nil
}

func unsafeGitConfigName(name string) bool {
	if name == "include.path" || strings.HasPrefix(name, "includeif.") || strings.HasPrefix(name, "url.") ||
		strings.HasPrefix(name, "http.") || strings.HasPrefix(name, "credential.") || strings.HasPrefix(name, "protocol.") ||
		strings.HasPrefix(name, "push.") || strings.HasPrefix(name, "filter.") || strings.HasPrefix(name, "submodule.") ||
		name == "core.askpass" || name == "core.hookspath" || name == "core.gitproxy" || name == "core.sshcommand" || name == "core.fsmonitor" {
		return true
	}
	return strings.HasPrefix(name, "remote.") && strings.HasSuffix(name, ".proxy")
}

func validateRequest(request Request) error {
	if err := request.Publication.ValidateAgainst(request.WorkspaceRepository, request.Task, request.Result, request.Verification); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if strings.TrimSpace(request.Title) == "" || strings.TrimSpace(request.Title) != request.Title || len(request.Title) > maxPullTitleBytes || !utf8.ValidString(request.Title) || hasControl(request.Title) || len(request.Body) > maxPullBodyBytes || !utf8.ValidString(request.Body) || strings.ContainsRune(request.Body, 0) {
		return ErrInvalidRequest
	}
	return nil
}

func hasControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func (publisher *Publisher) readBranch(ctx context.Context, identity githubapp.RepositoryIdentity, publication task.PublicationTuple) (task.GitOID, bool, error) {
	branch, err := publisher.repositories.BranchReference(ctx, identity, publication.RepositoryFullName, publication.Branch)
	if err != nil {
		var httpError *githubapp.HTTPError
		if errors.As(err, &httpError) && httpError.StatusCode() == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return task.GitOID(branch.SHA()), true, nil
}

func reconcileBranch(publication task.PublicationTuple, remote task.GitOID, exists bool) (bool, error) {
	if exists && remote == publication.ResultCommit {
		return false, nil
	}
	if publication.ExpectedRemoteOldSHA == "" {
		if exists {
			return false, ErrBranchConflict
		}
		return true, nil
	}
	if !exists || remote != publication.ExpectedRemoteOldSHA {
		return false, ErrBranchConflict
	}
	return true, nil
}

func (publisher *Publisher) findPullRequest(ctx context.Context, identity githubapp.RepositoryIdentity, publication task.PublicationTuple) (int64, bool, error) {
	pulls, err := publisher.repositories.FindOpenDraftPullRequests(ctx, identity, publication.RepositoryFullName, publication.BaseRef, publication.Branch)
	if err != nil {
		return 0, false, err
	}
	if len(pulls) == 0 {
		return 0, false, nil
	}
	if len(pulls) != 1 {
		return 0, false, githubapp.ErrAmbiguousPullRequests
	}
	return pulls[0].Number(), true, nil
}

func (publisher *Publisher) readPullRequest(ctx context.Context, identity githubapp.RepositoryIdentity, publication task.PublicationTuple) (PullRequestProof, error) {
	remoteSHA, exists, err := publisher.readBranch(ctx, identity, publication)
	if err != nil {
		return PullRequestProof{}, err
	}
	if !exists || remoteSHA != publication.ResultCommit {
		return PullRequestProof{}, ErrBranchConflict
	}
	number, found, err := publisher.findPullRequest(ctx, identity, publication)
	if err != nil {
		return PullRequestProof{}, err
	}
	if !found {
		return PullRequestProof{Found: false}, nil
	}
	remotePull, err := publisher.repositories.PullRequest(ctx, identity, publication.RepositoryFullName, number)
	if err != nil {
		return PullRequestProof{}, err
	}
	observation := publicationObservation(remoteSHA, remotePull)
	if err := observation.ValidateAgainst(publication); err != nil {
		return PullRequestProof{}, ErrPullRequestConflict
	}
	return PullRequestProof{Observation: observation, Found: true}, nil
}

func publicationObservation(remoteSHA task.GitOID, pull githubapp.PullRequestObservation) task.PublicationObservation {
	base := pull.Base()
	head := pull.Head()
	return task.PublicationObservation{
		RemoteSHA: remoteSHA,
		PullRequest: task.PullRequestObservation{
			RepositoryID: task.RepositoryID(pull.TargetRepositoryID()), RepositoryFullName: pull.TargetRepositoryFullName(),
			Number: task.PullRequestNumber(pull.Number()), URL: pull.HTMLURL(), State: pull.State(), Draft: pull.Draft(),
			BaseRepositoryID: task.RepositoryID(base.RepositoryID()), BaseRepositoryFullName: base.RepositoryFullName(), BaseRef: base.Ref(), BaseSHA: task.GitOID(base.SHA()),
			HeadRepositoryID: task.RepositoryID(head.RepositoryID()), HeadRepositoryFullName: head.RepositoryFullName(), HeadRepositoryOwner: head.RepositoryOwner(), HeadRepositoryName: head.RepositoryName(), HeadRef: head.Ref(), HeadSHA: task.GitOID(head.SHA()),
		},
	}
}
