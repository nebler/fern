package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed fixtures/tasks.json
var fixtureFiles embed.FS

var taskStates = map[string]bool{
	"queued": true, "running": true, "input_required": true,
	"cancel_requested": true, "uncertain": true, "recovery_required": true,
	"completed": true, "failed": true, "canceled": true,
}

var attemptStates = map[string]bool{
	"prepared": true, "delivering": true, "admitted": true, "running": true,
	"input_required": true, "cancel_requested": true, "uncertain": true,
	"recovery_required": true, "succeeded": true, "failed": true,
	"canceled": true, "superseded": true,
}

var approvalStates = map[string]bool{
	"pending": true, "decision_recorded": true, "delivering": true,
	"uncertain": true, "recovery_required": true, "applied": true,
	"rejected": true, "expired": true, "canceled": true,
}

var resultStates = map[string]bool{
	"collecting": true, "uncertain": true, "recovery_required": true,
	"sealed": true, "failed": true,
}

var verificationStates = map[string]bool{
	"requested": true, "running": true, "succeeded": true, "failed": true,
	"cancel_requested": true, "uncertain": true, "recovery_required": true,
	"canceled": true,
}

var publicationStates = map[string]bool{
	"requested": true, "preparing": true, "ready": true, "pushing": true,
	"opening_pr": true, "reconciling": true, "cancel_requested": true,
	"uncertain": true, "recovery_required": true, "published": true,
	"failed": true, "conflict": true, "canceled": true,
}

type FixtureSet struct {
	Workspace Workspace `json:"workspace"`
	Tasks     []Task    `json:"tasks"`
}

type Workspace struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	RepositoryID   string `json:"repositoryId"`
	RepositoryName string `json:"repositoryFullName"`
}

// Task contains TaskSummary fields plus the documented detail projection.
type Task struct {
	ID                   string         `json:"id"`
	Title                string         `json:"title"`
	State                string         `json:"state"`
	RepositoryID         string         `json:"repositoryId"`
	RepositoryFullName   string         `json:"repositoryFullName"`
	BaseRef              string         `json:"baseRef"`
	BaseSHA              string         `json:"baseSha"`
	CurrentAttemptID     string         `json:"currentAttemptId"`
	PendingApprovalCount int            `json:"pendingApprovalCount"`
	ResultID             *string        `json:"resultId"`
	Publication          *Publication   `json:"publication"`
	Revision             int            `json:"revision"`
	CreatedAt            time.Time      `json:"createdAt"`
	UpdatedAt            time.Time      `json:"updatedAt"`
	Links                Links          `json:"links"`
	Attempts             []Attempt      `json:"attempts"`
	Approvals            []Approval     `json:"approvals"`
	Cancellation         *Cancellation  `json:"cancellation"`
	Result               *Result        `json:"result"`
	Verifications        []Verification `json:"verifications"`
	LatestEventCursor    string         `json:"latestEventCursor"`
	EventNote            string         `json:"eventNote"`
}

type Links struct {
	Self     string `json:"self"`
	OpenCode string `json:"opencode"`
}

type Attempt struct {
	ID                string     `json:"id"`
	Sequence          int        `json:"sequence"`
	State             string     `json:"state"`
	OpenCodeSessionID string     `json:"opencodeSessionId"`
	StartedAt         *time.Time `json:"startedAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type Approval struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Summary   string    `json:"summary"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Cancellation struct {
	Epoch       int       `json:"epoch"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requestedAt"`
}

type Result struct {
	ID            string        `json:"id"`
	State         string        `json:"state"`
	Outcome       string        `json:"outcome"`
	BaseSHA       string        `json:"baseSha"`
	ResultCommit  string        `json:"resultCommit"`
	ManifestSHA   string        `json:"manifestSha256"`
	CleanWorktree bool          `json:"cleanWorktree"`
	ChangedFiles  []ChangedFile `json:"changedFiles"`
}

type ChangedFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int    `json:"size"`
}

type Verification struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Check          string `json:"check"`
	VerifiedCommit string `json:"verifiedCommit"`
	ExitStatus     *int   `json:"exitStatus"`
}

type Publication struct {
	ID           string       `json:"id"`
	OperationID  string       `json:"operationId"`
	State        string       `json:"state"`
	Branch       string       `json:"branch"`
	ResultCommit string       `json:"resultCommit"`
	PullRequest  *PullRequest `json:"pullRequest"`
	UpdatedAt    time.Time    `json:"updatedAt"`
}

type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Draft  bool   `json:"draft"`
}

func loadFixtures(files fs.FS, name string) (FixtureSet, error) {
	b, err := fs.ReadFile(files, name)
	if err != nil {
		return FixtureSet{}, err
	}
	var set FixtureSet
	if err := json.Unmarshal(b, &set); err != nil {
		return FixtureSet{}, fmt.Errorf("decode fixtures: %w", err)
	}
	if err := set.validate(); err != nil {
		return FixtureSet{}, err
	}
	sort.SliceStable(set.Tasks, func(i, j int) bool {
		return set.Tasks[i].UpdatedAt.After(set.Tasks[j].UpdatedAt)
	})
	return set, nil
}

func (s FixtureSet) validate() error {
	seen := make(map[string]bool)
	for _, task := range s.Tasks {
		if seen[task.ID] {
			return fmt.Errorf("duplicate task %q", task.ID)
		}
		seen[task.ID] = true
		if !taskStates[task.State] {
			return fmt.Errorf("task %s has undocumented state %q", task.ID, task.State)
		}
		if !validOpenCodeLink(task.Links.OpenCode) {
			return fmt.Errorf("task %s has invalid OpenCode deep link", task.ID)
		}
		if !validGitSHA(task.BaseSHA) {
			return fmt.Errorf("task %s has invalid base SHA", task.ID)
		}
		for _, attempt := range task.Attempts {
			if !attemptStates[attempt.State] {
				return fmt.Errorf("attempt %s has undocumented state %q", attempt.ID, attempt.State)
			}
		}
		for _, approval := range task.Approvals {
			if !approvalStates[approval.State] {
				return fmt.Errorf("approval %s has undocumented state %q", approval.ID, approval.State)
			}
		}
		if task.Result != nil && !resultStates[task.Result.State] {
			return fmt.Errorf("result %s has undocumented state %q", task.Result.ID, task.Result.State)
		}
		if task.Result != nil && (!validGitSHA(task.Result.BaseSHA) || !validGitSHA(task.Result.ResultCommit)) {
			return fmt.Errorf("result %s has invalid commit identity", task.Result.ID)
		}
		for _, verification := range task.Verifications {
			if !verificationStates[verification.State] {
				return fmt.Errorf("verification %s has undocumented state %q", verification.ID, verification.State)
			}
		}
		if task.Publication != nil && !publicationStates[task.Publication.State] {
			return fmt.Errorf("publication %s has undocumented state %q", task.Publication.ID, task.Publication.State)
		}
		if task.Publication != nil && task.Publication.PullRequest != nil && !validPullRequest(*task.Publication.PullRequest, task.RepositoryFullName) {
			return fmt.Errorf("publication %s has an invalid pull request link", task.Publication.ID)
		}
	}
	return nil
}

func validPullRequest(pull PullRequest, repository string) bool {
	parsed, err := url.Parse(pull.URL)
	if err != nil || pull.Number <= 0 || !pull.Draft || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Path == "/"+repository+"/pull/"+strconv.Itoa(pull.Number)
}

func validOpenCodeLink(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	sessionID := strings.TrimPrefix(parsed.Path, "/session/")
	return sessionID != parsed.Path && strings.HasPrefix(sessionID, "ses") && !strings.Contains(sessionID, "/")
}

func validGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
