package taskenvdocker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/gitref"
	"github.com/nebler/fern/internal/taskstore"
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

// A lease records successful delivery, never the credential itself. A provider
// restart therefore always mints fresh credentials, even for the same runtime.
type githubCredentialLease struct {
	digest, runtime string
	repository      githubapp.RepositoryIdentity
	expires         time.Time
}

// RefreshGitHubCredentials supplies the harness with a short-lived, one-repo
// App token. Call after healthy runtime commitment and before session admission,
// and early in every working scan. A failed refresh prevents new prompt
// admission and is reported for retry; an already-running harness may still use
// its previous token until GitHub expires it. A failed refresh is never cached.
func (p *Provider) RefreshGitHubCredentials(ctx context.Context, run taskstore.BackgroundRun) error {
	if p.config.GitHubTokens == nil {
		return nil
	}
	p.lifecycle.githubMu.Lock()
	defer p.lifecycle.githubMu.Unlock()
	p.lifecycle.mu.Lock()
	closed := p.lifecycle.closed
	p.lifecycle.mu.Unlock()
	if closed {
		return ErrProviderClosed
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
	if err != nil || identity != p.config.GitHubRepository || gitref.ValidateOwnerRepo(p.config.GitHubRepositoryFullName) != nil || run.RepositoryRemote != "https://github.com/"+p.config.GitHubRepositoryFullName {
		return errors.New("GitHub credential repository identity mismatch")
	}
	api, ok := p.docker.(githubCredentialDocker)
	if !ok {
		return errors.New("Docker GitHub credential delivery is unavailable")
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	defer cancel()
	attest := func() error {
		if operation.Err() != nil {
			return errors.New("GitHub credential operation cancelled")
		}
		info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
		if err != nil {
			return errors.New("inspect GitHub credential runtime failed")
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
	lease := p.githubCredential
	if lease.digest == digest && lease.runtime == runtime.Token && lease.repository == identity && now().Add(githubRefreshAhead).Before(lease.expires) {
		return nil
	}
	// Invalidate the lease before any fallible refresh work.
	p.githubCredential = githubCredentialLease{}
	token, err := p.config.GitHubTokens.InstallationToken(operation, identity)
	if err != nil {
		return errors.New("mint GitHub runtime credential failed")
	}
	if token.Identity() != identity || token.Permissions().Contents() != "write" || token.Permissions().PullRequests() != "write" || !now().Add(githubRefreshAhead).Before(token.ExpiresAt()) {
		return errors.New("invalid GitHub runtime credential")
	}
	secret, err := token.Value(now())
	if err != nil || len(secret) > 4096 || strings.ContainsAny(secret, "\x00\r\n") {
		return errors.New("invalid GitHub runtime credential")
	}
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for _, file := range []struct{ name, body string }{
		{".fern-github-token-staging", secret},
		{".fern-github-repository-staging", p.config.GitHubRepositoryFullName + "\n"},
	} {
		if err := w.WriteHeader(&tar.Header{Name: file.name, Mode: 0600, Uid: 1001, Gid: 1001, Size: int64(len(file.body)), Typeflag: tar.TypeReg}); err != nil {
			return errors.New("encode GitHub runtime credential failed")
		}
		if _, err := io.WriteString(w, file.body); err != nil {
			return errors.New("encode GitHub runtime credential failed")
		}
	}
	if err := w.Close(); err != nil {
		return errors.New("encode GitHub runtime credential failed")
	}
	// Minting can take time: inspect the exact running process again immediately
	// before each mutation, always addressing writes by immutable container ID.
	if err := attest(); err != nil {
		return err
	}
	if err := api.CopyToContainer(operation, runtime.ContainerID, opencodeTarget, &archive, container.CopyToContainerOptions{CopyUIDGID: true, AllowOverwriteDirWithFile: false}); err != nil {
		return errors.New("stage GitHub runtime credential failed")
	}
	if err := attest(); err != nil {
		return err
	}
	exec, err := api.ContainerExecCreate(operation, runtime.ContainerID, container.ExecOptions{
		User: containerUser, WorkingDir: opencodeTarget,
		Cmd: []string{"/bin/sh", "-c", "/bin/mv -fT -- .fern-github-repository-staging fern-github-repository && /bin/mv -fT -- .fern-github-token-staging fern-github-token"},
	})
	if err != nil || exec.ID == "" {
		return errors.New("prepare GitHub runtime credential installation failed")
	}
	if err := attest(); err != nil {
		return err
	}
	if err := api.ContainerExecStart(operation, exec.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return errors.New("install GitHub runtime credential failed")
	}
	for {
		result, err := api.ContainerExecInspect(operation, exec.ID)
		if err != nil {
			return errors.New("inspect GitHub runtime credential installation failed")
		}
		if result.ContainerID != runtime.ContainerID {
			return errors.New("GitHub credential installation container mismatch")
		}
		if !result.Running {
			if result.ExitCode != 0 {
				return errors.New("GitHub runtime credential installation failed")
			}
			break
		}
		select {
		case <-operation.Done():
			return errors.New("GitHub runtime credential installation timed out")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := attest(); err != nil {
		return err
	}
	if !now().Add(githubRefreshAhead).Before(token.ExpiresAt()) {
		return errors.New("GitHub runtime credential expired during installation")
	}
	p.githubCredential = githubCredentialLease{digest: digest, runtime: runtime.Token, repository: identity, expires: token.ExpiresAt()}
	return nil
}
