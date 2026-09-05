package task

import (
	"fmt"
)

// RepositoryTuple identifies the immutable repository/base pair a task is
// bound to.
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

// ResultOutcome classifies a sealed result: changed work or an explicit
// no-op against base.
type ResultOutcome string

const (
	ResultChanged   ResultOutcome = "changed"
	ResultNoChanges ResultOutcome = "no_changes"
)

// ResultTuple is the sealed outcome of one attempt against its task's
// repository pair.
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
