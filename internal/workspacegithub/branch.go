package workspacegithub

import (
	"context"
	"errors"
)

var (
	ErrBranchConflict = errors.New("GitHub branch conflicts with the authorized tuple")
	ErrPushUnknown    = errors.New("GitHub branch mutation could not be observed")
)

type BranchRequest struct {
	RepositoryFullName string
	RepositoryID       int64
	BaseRef            string
	BaseSHA            string
	HeadRef            string
	HeadSHA            string
}

type BranchObservation struct {
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
	Exists bool   `json:"-"`
}

type BranchProof struct {
	Observation BranchObservation
	Attempted   bool
}

// ReconcileBranch proves the configured repository and immutable base before
// reading the exact Fern publication ref. It performs no mutation.
func (c *Client) ReconcileBranch(ctx context.Context, request BranchRequest) (BranchObservation, error) {
	if err := c.validateBranchRequest(ctx, request); err != nil {
		return BranchObservation{}, err
	}
	return c.readBranch(ctx, request)
}

// PushBranchOnce creates an absent Fern branch at the exact authorized commit.
// It never updates or force-pushes. A create response, including an error, is
// followed by an exact read before the outcome is classified.
func (c *Client) PushBranchOnce(ctx context.Context, request BranchRequest) (BranchProof, error) {
	var proof BranchProof
	if err := c.validateBranchRequest(ctx, request); err != nil {
		return proof, err
	}
	observed, err := c.readBranch(ctx, request)
	if err != nil {
		return proof, err
	}
	proof.Observation = observed
	if observed.Exists {
		if observed.SHA == request.HeadSHA {
			return proof, nil
		}
		return proof, ErrBranchConflict
	}

	proof.Attempted = true
	_, createErr := c.executor.Run(ctx, "api", "--hostname", c.hostname, "--method", "POST",
		"repos/"+request.RepositoryFullName+"/git/refs", "--raw-field", "ref=refs/heads/"+request.HeadRef,
		"--raw-field", "sha="+request.HeadSHA, "--jq", `{ref: .ref, sha: .object.sha}`)
	observed, readErr := c.readBranch(ctx, request)
	proof.Observation = observed
	if readErr != nil {
		if ctx.Err() != nil {
			return proof, ctx.Err()
		}
		return proof, ErrPushUnknown
	}
	if observed.Exists && observed.SHA == request.HeadSHA {
		return proof, nil
	}
	if observed.Exists {
		return proof, ErrBranchConflict
	}
	if createErr != nil {
		return proof, ErrPushUnknown
	}
	return proof, ErrPushUnknown
}

func (c *Client) validateBranchRequest(ctx context.Context, request BranchRequest) error {
	if !validRepository(request.RepositoryFullName) || request.RepositoryID <= 0 ||
		!validGitRef(request.BaseRef) || !validGitRef(request.HeadRef) || request.BaseRef == request.HeadRef ||
		!validGitOID(request.BaseSHA) || !validGitOID(request.HeadSHA) || request.BaseSHA == request.HeadSHA {
		return ErrBranchConflict
	}
	base, err := c.Branch(ctx, request.RepositoryID, request.RepositoryFullName, request.BaseRef)
	if err != nil {
		if errors.Is(err, ErrRepository) {
			return ErrBranchConflict
		}
		return ErrPushUnknown
	}
	if base.SHA != request.BaseSHA {
		return ErrBranchConflict
	}
	return nil
}

func (c *Client) readBranch(ctx context.Context, request BranchRequest) (BranchObservation, error) {
	output, err := c.executor.Run(ctx, "api", "--hostname", c.hostname, "--method", "GET",
		"repos/"+request.RepositoryFullName+"/git/matching-refs/heads/"+request.HeadRef,
		"--jq", `[.[] | {ref: .ref, sha: .object.sha}]`)
	if err != nil {
		if ctx.Err() != nil {
			return BranchObservation{}, ctx.Err()
		}
		return BranchObservation{}, ErrPushUnknown
	}
	var candidates []BranchObservation
	if err := decodeBounded(output, &candidates); err != nil {
		return BranchObservation{}, ErrPushUnknown
	}
	var observed *BranchObservation
	for index := range candidates {
		candidate := candidates[index]
		if candidate.Ref != "refs/heads/"+request.HeadRef {
			continue
		}
		if observed != nil || !validGitOID(candidate.SHA) {
			return BranchObservation{}, ErrBranchConflict
		}
		observed = &candidate
	}
	if observed == nil {
		return BranchObservation{}, nil
	}
	observed.Exists = true
	return *observed, nil
}
