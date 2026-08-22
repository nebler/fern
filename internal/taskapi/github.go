package taskapi

import (
	"context"
	"errors"
	"time"

	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/task"
)

type GitHubReferenceReader interface {
	BranchReference(context.Context, githubapp.RepositoryIdentity, string, string) (githubapp.GitReferenceObservation, error)
}

// GitHubBaseResolver binds task admission to an exact repository-scoped
// GitHub branch observation with a bounded request lifetime.
func GitHubBaseResolver(reader GitHubReferenceReader, identity githubapp.RepositoryIdentity, fullName string, timeout time.Duration) (BaseResolver, error) {
	if reader == nil || identity.InstallationID() <= 0 || identity.RepositoryID() <= 0 || fullName == "" || timeout <= 0 || timeout > time.Minute {
		return nil, errors.New("valid GitHub base resolver configuration is required")
	}
	return func(parent context.Context, baseRef string) (task.GitOID, error) {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		observation, err := reader.BranchReference(ctx, identity, fullName, baseRef)
		if err != nil {
			return "", err
		}
		if observation.Identity() != identity || observation.Ref() != baseRef {
			return "", errors.New("GitHub base observation conflicts with configured authority")
		}
		return task.ParseGitOID(observation.SHA())
	}, nil
}
