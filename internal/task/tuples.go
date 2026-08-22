package task

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type RepositoryTuple struct {
	RepositoryID RepositoryID
	BaseSHA      GitOID
}

func (t RepositoryTuple) Validate() error {
	if t.RepositoryID == 0 || uint64(t.RepositoryID) > maxSQLiteInteger {
		return fmt.Errorf("%w: repository ID", ErrInvalidTuple)
	}
	if _, err := ParseGitOID(string(t.BaseSHA)); err != nil {
		return fmt.Errorf("%w: base SHA", ErrInvalidTuple)
	}
	return nil
}

type ResultOutcome string

const (
	ResultChanged   ResultOutcome = "changed"
	ResultNoChanges ResultOutcome = "no_changes"
)

type ResultTuple struct {
	RepositoryTuple
	ResultCommit    GitOID
	Outcome         ResultOutcome
	ManifestEntries int
	WorktreeClean   bool
}

// ValidateAgainst verifies the immutable task/result values and invariants
// knowable without Git. The caller must separately prove object existence,
// ancestry, manifest contents, and the synchronized OpenCode boundary.
func (r ResultTuple) ValidateAgainst(task RepositoryTuple) error {
	if err := task.Validate(); err != nil {
		return err
	}
	if err := r.RepositoryTuple.Validate(); err != nil {
		return err
	}
	if r.RepositoryTuple != task {
		return fmt.Errorf("%w: result repository/base differs from task", ErrInvalidTuple)
	}
	if _, err := ParseGitOID(string(r.ResultCommit)); err != nil {
		return fmt.Errorf("%w: result commit", ErrInvalidTuple)
	}
	if !r.WorktreeClean || r.ManifestEntries < 0 {
		return fmt.Errorf("%w: dirty worktree or invalid manifest count", ErrInvalidTuple)
	}
	switch r.Outcome {
	case ResultNoChanges:
		if r.ResultCommit != r.BaseSHA || r.ManifestEntries != 0 {
			return fmt.Errorf("%w: no_changes must seal base with empty manifest", ErrInvalidTuple)
		}
	case ResultChanged:
		if r.ResultCommit == r.BaseSHA || r.ManifestEntries == 0 {
			return fmt.Errorf("%w: changed result must differ and have a manifest", ErrInvalidTuple)
		}
	default:
		return fmt.Errorf("%w: result outcome", ErrInvalidTuple)
	}
	return nil
}

type VerificationTuple struct {
	State          VerificationState
	VerifiedCommit GitOID
}

type PublicationTuple struct {
	OperationID        PublicationOperationID
	InstallationID     InstallationID
	RepositoryID       RepositoryID
	RepositoryFullName string
	WorkspaceName      string
	BaseRef            string
	BaseSHA            GitOID
	ResultCommit       GitOID
	Branch             string
	// ExpectedRemoteOldSHA is empty when the operation expects no remote branch.
	ExpectedRemoteOldSHA GitOID
}

// ValidateAgainst proves publication inputs refer to one workspace, task,
// result, and successful verification. Current policy eligibility remains a
// caller-owned admission check.
func (p PublicationTuple) ValidateAgainst(workspaceRepository RepositoryID, task RepositoryTuple, result ResultTuple, verification VerificationTuple) error {
	if workspaceRepository == 0 || uint64(workspaceRepository) > maxSQLiteInteger || p.InstallationID == 0 || uint64(p.InstallationID) > maxSQLiteInteger {
		return fmt.Errorf("%w: repository or installation ID", ErrInvalidTuple)
	}
	if err := result.ValidateAgainst(task); err != nil {
		return err
	}
	if _, err := ParsePublicationOperationID(string(p.OperationID)); err != nil {
		return fmt.Errorf("%w: operation ID", ErrInvalidTuple)
	}
	if p.RepositoryID != workspaceRepository || p.RepositoryID != task.RepositoryID || p.BaseSHA != task.BaseSHA || p.ResultCommit != result.ResultCommit {
		return fmt.Errorf("%w: publication values differ from workspace/task/result", ErrInvalidTuple)
	}
	if _, _, err := splitRepositoryFullName(p.RepositoryFullName); err != nil {
		return err
	}
	if p.ExpectedRemoteOldSHA != "" {
		if _, err := ParseGitOID(string(p.ExpectedRemoteOldSHA)); err != nil {
			return fmt.Errorf("%w: expected remote old SHA", ErrInvalidTuple)
		}
	}
	if verification.State != VerificationSucceeded || verification.VerifiedCommit != p.ResultCommit {
		return fmt.Errorf("%w: verification does not prove result commit", ErrInvalidTuple)
	}
	if p.WorkspaceName == "" || hasControl(p.WorkspaceName) || p.BaseRef == "" || hasControl(p.BaseRef) {
		return fmt.Errorf("%w: base ref or branch", ErrInvalidTuple)
	}
	if p.Branch != PublicationBranch(p.WorkspaceName, p.OperationID) || p.Branch == p.BaseRef {
		return fmt.Errorf("%w: branch does not match operation", ErrInvalidTuple)
	}
	return nil
}

// PublicationBranch returns the only branch name permitted by the task model.
// Workspace-name Git ref safety is a workspace-configuration responsibility.
func PublicationBranch(workspaceName string, operationID PublicationOperationID) string {
	return "fern/" + workspaceName + "/" + string(operationID)
}

type PullRequestObservation struct {
	RepositoryID           RepositoryID
	RepositoryFullName     string
	Number                 PullRequestNumber
	URL                    string
	State                  string
	Draft                  bool
	BaseRepositoryID       RepositoryID
	BaseRepositoryFullName string
	BaseRef                string
	BaseSHA                GitOID
	HeadRepositoryID       RepositoryID
	HeadRepositoryFullName string
	HeadRepositoryOwner    string
	HeadRepositoryName     string
	HeadRef                string
	HeadSHA                GitOID
}

type PublicationObservation struct {
	RemoteSHA   GitOID
	PullRequest PullRequestObservation
}

func (o PublicationObservation) ValidateAgainst(p PublicationTuple) error {
	owner, name, err := splitRepositoryFullName(p.RepositoryFullName)
	if err != nil {
		return err
	}
	pr := o.PullRequest
	if _, err := ParsePullRequestNumber(strconv.FormatUint(uint64(pr.Number), 10)); err != nil {
		return fmt.Errorf("%w: pull request number", ErrInvalidTuple)
	}
	if _, err := ParseGitOID(string(o.RemoteSHA)); err != nil {
		return fmt.Errorf("%w: remote SHA", ErrInvalidTuple)
	}
	if _, err := ParseGitOID(string(pr.BaseSHA)); err != nil {
		return fmt.Errorf("%w: pull request base SHA", ErrInvalidTuple)
	}
	if _, err := ParseGitOID(string(pr.HeadSHA)); err != nil {
		return fmt.Errorf("%w: pull request head SHA", ErrInvalidTuple)
	}
	if o.RemoteSHA != p.ResultCommit || pr.RepositoryID != p.RepositoryID || pr.RepositoryFullName != p.RepositoryFullName ||
		pr.State != "open" || !pr.Draft ||
		pr.BaseRepositoryID != p.RepositoryID || pr.BaseRepositoryFullName != p.RepositoryFullName || pr.BaseRef != p.BaseRef || pr.BaseSHA != p.BaseSHA ||
		pr.HeadRepositoryID != p.RepositoryID || pr.HeadRepositoryFullName != p.RepositoryFullName || pr.HeadRepositoryOwner != owner || pr.HeadRepositoryName != name ||
		pr.HeadRef != p.Branch || pr.HeadSHA != p.ResultCommit || !validPullRequestURL(pr.URL, p.RepositoryFullName, pr.Number) {
		return fmt.Errorf("%w: remote publication observation", ErrInvalidTuple)
	}
	return nil
}

func splitRepositoryFullName(fullName string) (string, string, error) {
	if len(fullName) < 3 || len(fullName) > 201 || strings.Count(fullName, "/") != 1 || hasControl(fullName) || strings.TrimSpace(fullName) != fullName {
		return "", "", fmt.Errorf("%w: repository full name", ErrInvalidTuple)
	}
	owner, name, _ := strings.Cut(fullName, "/")
	if !validRepositoryOwner(owner) || !validRepositoryName(name) || strings.HasSuffix(strings.ToLower(name), ".git") {
		return "", "", fmt.Errorf("%w: repository full name", ErrInvalidTuple)
	}
	return owner, name, nil
}

func validRepositoryOwner(value string) bool {
	if len(value) < 1 || len(value) > 39 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for index := range len(value) {
		char := value[index]
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func validRepositoryName(value string) bool {
	if len(value) < 1 || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for index := range len(value) {
		char := value[index]
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func validPullRequestURL(raw, repositoryFullName string, number PullRequestNumber) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return false
	}
	wantPath := "/" + repositoryFullName + "/pull/" + strconv.FormatUint(uint64(number), 10)
	return parsed.Path == wantPath
}
