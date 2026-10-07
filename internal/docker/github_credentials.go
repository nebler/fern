package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/store"
)

const githubRefreshAhead = 5 * time.Minute

// Keep credential writes separate from ordinary lifecycle operations so nil
// token sources remain usable with hermetic lifecycle-only Docker fakes.
type githubCredentialDocker interface {
	CopyToContainer(context.Context, string, string, io.Reader, container.CopyToContainerOptions) error
	ContainerExecCreate(context.Context, string, container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecStart(context.Context, string, container.ExecStartOptions) error
	ContainerExecInspect(context.Context, string) (container.ExecInspect, error)
}

// githubCredentialCache records the last successful delivery, never the
// credential itself. A provider restart therefore always mints fresh
// credentials, even for the same runtime.
type githubCredentialCache struct {
	digest, runtime string
	repository      githubapp.RepositoryIdentity
	expires         time.Time
}

// RefreshGitHubCredentials supplies the harness with a short-lived, one-repo
// App token. Call after healthy runtime commitment and before session admission,
// and early in every working scan. A failed refresh prevents new prompt
// admission and is reported for retry; an already-running harness may still use
// its previous token until GitHub expires it. A failed refresh is never cached.
// Token bytes never appear in errors.
func (p *Provider) RefreshGitHubCredentials(ctx context.Context, run store.BackgroundRun) error {
	if p.config.GitHubTokens == nil {
		return nil
	}
	p.lifecycle.githubMu.Lock()
	defer p.lifecycle.githubMu.Unlock()
	if err := p.requireOpen(); err != nil {
		return err
	}
	digest, err := p.validateRun(run)
	if err != nil {
		return err
	}
	runtime, err := committedRuntimeFromRun(run)
	if err != nil {
		return err
	}
	identity, err := githubapp.NewRepositoryIdentity(p.config.GitHubRepository.InstallationID(), int64(run.RepositoryID))
	if err != nil || identity != p.config.GitHubRepository || run.RepositoryRemote != "https://github.com/"+p.config.GitHubRepositoryFullName {
		return errors.New("GitHub credential repository identity mismatch")
	}
	api, ok := p.docker.(githubCredentialDocker)
	if !ok {
		return errors.New("Docker GitHub credential delivery is unavailable")
	}
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	// Minting can take time: inspect the exact running process again
	// immediately before each mutation, always addressing writes by immutable
	// container ID.
	attest := func() error {
		if err := operation.Err(); err != nil {
			return fmt.Errorf("GitHub credential operation cancelled: %w", err)
		}
		info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
		if err != nil {
			return fmt.Errorf("inspect GitHub credential runtime: %w", err)
		}
		if err := p.attestContainer(run, digest, info, true); err != nil {
			return &IdentityError{Resource: "container", Identity: run.ContainerIdentity, Reason: "GitHub credential runtime attestation failed"}
		}
		return requireRuntime(info, runtime)
	}
	if err := attest(); err != nil {
		return err
	}
	now := p.githubNow
	if now == nil {
		now = time.Now
	}
	cached := p.githubCredential
	if cached.digest == digest && cached.runtime == runtime.Token && cached.repository == identity && now().Add(githubRefreshAhead).Before(cached.expires) {
		return nil
	}
	// Invalidate the cache before any fallible refresh work.
	p.githubCredential = githubCredentialCache{}
	token, archive, err := p.mintGitHubCredential(operation, identity, now)
	if err != nil {
		return err
	}
	if err := installGitHubCredential(operation, api, runtime.ContainerID, archive, attest); err != nil {
		return err
	}
	if !now().Add(githubRefreshAhead).Before(token.ExpiresAt()) {
		return errors.New("GitHub runtime credential expired during installation")
	}
	p.githubCredential = githubCredentialCache{digest: digest, runtime: runtime.Token, repository: identity, expires: token.ExpiresAt()}
	return nil
}

// mintGitHubCredential mints and checks a token for identity and encodes it,
// with the repository name, as a tar of two staging files.
func (p *Provider) mintGitHubCredential(ctx context.Context, identity githubapp.RepositoryIdentity, now func() time.Time) (githubapp.InstallationToken, *bytes.Buffer, error) {
	token, err := p.config.GitHubTokens.InstallationToken(ctx, identity)
	if err != nil {
		return githubapp.InstallationToken{}, nil, errors.New("mint GitHub runtime credential failed")
	}
	if token.Identity() != identity || token.Permissions().Contents() != "write" || token.Permissions().PullRequests() != "write" || !now().Add(githubRefreshAhead).Before(token.ExpiresAt()) {
		return githubapp.InstallationToken{}, nil, errors.New("invalid GitHub runtime credential")
	}
	secret, err := token.Value(now())
	if err != nil || len(secret) > 4096 || strings.ContainsAny(secret, "\x00\r\n") {
		return githubapp.InstallationToken{}, nil, errors.New("invalid GitHub runtime credential")
	}
	archive := &bytes.Buffer{}
	w := tar.NewWriter(archive)
	for _, file := range []struct{ name, body string }{
		{".fern-github-token-staging", secret},
		{".fern-github-repository-staging", p.config.GitHubRepositoryFullName + "\n"},
	} {
		if err := w.WriteHeader(&tar.Header{Name: file.name, Mode: 0o600, Uid: 1001, Gid: 1001, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			return githubapp.InstallationToken{}, nil, errors.New("encode GitHub runtime credential failed")
		}
		if _, err := io.WriteString(w, file.body); err != nil {
			return githubapp.InstallationToken{}, nil, errors.New("encode GitHub runtime credential failed")
		}
	}
	if err := w.Close(); err != nil {
		return githubapp.InstallationToken{}, nil, errors.New("encode GitHub runtime credential failed")
	}
	return token, archive, nil
}

// installGitHubCredential copies the staged files into the container's
// OpenCode volume and renames them into place, re-attesting the runtime
// before and after each step.
func installGitHubCredential(ctx context.Context, api githubCredentialDocker, containerID string, archive io.Reader, attest func() error) error {
	if err := attest(); err != nil {
		return err
	}
	// The copy carries the token, so its error is not passed through.
	if err := api.CopyToContainer(ctx, containerID, opencodeTarget, archive, container.CopyToContainerOptions{CopyUIDGID: true, AllowOverwriteDirWithFile: false}); err != nil {
		return errors.New("stage GitHub runtime credential failed")
	}
	if err := attest(); err != nil {
		return err
	}
	exec, err := api.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		User: containerUser, WorkingDir: opencodeTarget,
		Cmd: []string{"/bin/sh", "-c", "/bin/mv -fT -- .fern-github-repository-staging fern-github-repository && /bin/mv -fT -- .fern-github-token-staging fern-github-token"},
	})
	if err != nil {
		return fmt.Errorf("prepare GitHub runtime credential installation: %w", err)
	}
	if exec.ID == "" {
		return errors.New("prepare GitHub runtime credential installation: Docker returned no exec ID")
	}
	if err := attest(); err != nil {
		return err
	}
	if err := api.ContainerExecStart(ctx, exec.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("install GitHub runtime credential: %w", err)
	}
	for {
		result, err := api.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			return fmt.Errorf("inspect GitHub runtime credential installation: %w", err)
		}
		if result.ContainerID != containerID {
			return errors.New("GitHub credential installation container mismatch")
		}
		if !result.Running {
			if result.ExitCode != 0 {
				return fmt.Errorf("GitHub runtime credential installation exited %d", result.ExitCode)
			}
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("GitHub runtime credential installation timed out")
		case <-time.After(10 * time.Millisecond):
		}
	}
	return attest()
}
