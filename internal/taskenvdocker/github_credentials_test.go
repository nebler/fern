package taskenvdocker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/taskstore"
)

const githubTestSecret = "github_installation_token_private_12345"

type credentialDocker struct {
	*fakeDocker
	t             *testing.T
	copies, execs int
	copyError     error
}

func (d *credentialDocker) CopyToContainer(ctx context.Context, id, path string, body io.Reader, options container.CopyToContainerOptions) error {
	d.copies++
	if _, ok := ctx.Deadline(); !ok {
		d.t.Fatal("unbounded copy")
	}
	if id != d.info.ID || path != opencodeTarget || !options.CopyUIDGID || options.AllowOverwriteDirWithFile {
		d.t.Fatal("unsafe copy target/options")
	}
	reader := tar.NewReader(body)
	want := map[string]string{".fern-github-token-staging": githubTestSecret, ".fern-github-repository-staging": "fern-test/repository\n"}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			d.t.Fatal(err)
		}
		value, err := io.ReadAll(reader)
		if err != nil || header.Mode != 0600 || header.Uid != 1001 || header.Gid != 1001 || header.Typeflag != tar.TypeReg || string(value) != want[header.Name] {
			d.t.Fatal("unsafe credential archive")
		}
		delete(want, header.Name)
	}
	if len(want) != 0 {
		d.t.Fatal("incomplete archive")
	}
	return d.copyError
}

func (d *credentialDocker) ContainerExecCreate(ctx context.Context, id string, options container.ExecOptions) (container.ExecCreateResponse, error) {
	d.execs++
	encoded, _ := json.Marshal(options)
	if strings.Contains(string(encoded), githubTestSecret) || len(options.Env) != 0 || options.User != containerUser || options.WorkingDir != opencodeTarget || id != d.info.ID || options.AttachStdout || options.AttachStderr {
		d.t.Fatal("unsafe credential exec")
	}
	return container.ExecCreateResponse{ID: "credential-exec"}, nil
}
func (d *credentialDocker) ContainerExecStart(context.Context, string, container.ExecStartOptions) error {
	return nil
}
func (d *credentialDocker) ContainerExecInspect(context.Context, string) (container.ExecInspect, error) {
	return container.ExecInspect{ContainerID: d.info.ID, ExitCode: 0}, nil
}

type credentialAppSource struct{}

func (credentialAppSource) AppToken(time.Time) (string, error) {
	return "app.jwt.signature", nil
}

type credentialSourceFunc func(context.Context, githubapp.RepositoryIdentity) (githubapp.InstallationToken, error)

func (f credentialSourceFunc) InstallationToken(ctx context.Context, id githubapp.RepositoryIdentity) (githubapp.InstallationToken, error) {
	return f(ctx, id)
}

func githubCredentialFixture(t *testing.T) (*Provider, *credentialDocker, taskstore.BackgroundRun, *time.Time, *int) {
	t.Helper()
	p, base, run := preparedProvider(t)
	run.RepositoryID = 202
	created, err := p.EnsureContainer(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	started, err := p.StartContainer(context.Background(), run, created.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	run.ObservedContainerID, run.ObservedContainerStartedAt, run.RuntimeEpoch = started.ContainerID, started.ContainerStarted, started.RuntimeEpoch
	d := &credentialDocker{fakeDocker: base, t: t}
	p.docker = d
	now := time.Now().UTC().Truncate(time.Second)
	p.githubNow = func() time.Time { return now }
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/app/installations/101/access_tokens" {
			t.Error("wrong mint scope")
		}
		var body struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 202 || len(body.Permissions) != 2 || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" {
			t.Error("wrong token request body")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":{"contents":"write","pull_requests":"write","metadata":"read"}}`, githubTestSecret, now.Add(10*time.Minute).Format(time.RFC3339))
	}))
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL)
	httpClient := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		copy.URL = &u
		copy.URL.Scheme, copy.URL.Host = endpoint.Scheme, endpoint.Host
		return server.Client().Transport.RoundTrip(copy)
	})}
	source, err := githubapp.NewClient(httpClient, credentialAppSource{})
	if err != nil {
		t.Fatal(err)
	}
	p.config.GitHubTokens = source
	p.config.GitHubRepository, _ = githubapp.NewRepositoryIdentity(101, 202)
	p.config.GitHubRepositoryFullName = "fern-test/repository"
	return p, d, run, &now, &calls
}

func TestGitHubCredentialDeliveryRefreshAndRestart(t *testing.T) {
	p, d, run, now, calls := githubCredentialFixture(t)
	refresh := func() {
		t.Helper()
		if err := p.RefreshGitHubCredentials(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	refresh()
	if *calls != 1 || d.copies != 1 || d.execs != 1 {
		t.Fatal("cache missed")
	}
	*now = now.Add(5*time.Minute - time.Second)
	refresh()
	if *calls != 1 {
		t.Fatal("refreshed too early")
	}
	*now = now.Add(time.Second)
	refresh()
	if *calls != 2 || d.copies != 2 {
		t.Fatal("did not refresh five minutes early")
	}
	// Reconstructed providers retain no credential lease.
	p.githubCredential = githubCredentialCache{}
	refresh()
	if *calls != 3 {
		t.Fatal("restart reused credential")
	}
}

func TestGitHubCredentialRejectsChangedRuntimeAfterMint(t *testing.T) {
	p, d, run, _, _ := githubCredentialFixture(t)
	source := p.config.GitHubTokens
	p.config.GitHubTokens = credentialSourceFunc(func(ctx context.Context, id githubapp.RepositoryIdentity) (githubapp.InstallationToken, error) {
		token, err := source.InstallationToken(ctx, id)
		d.info.State.StartedAt = "2026-09-05T12:00:00.000000001Z"
		return token, err
	})
	if err := p.RefreshGitHubCredentials(context.Background(), run); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v", err)
	}
	if d.copies != 0 || d.execs != 0 {
		t.Fatal("mutated changed runtime")
	}
}

func TestGitHubCredentialInvalidSourceAndExpiredLeaseFailClosed(t *testing.T) {
	for _, scenario := range []string{"source error", "zero token", "wrong identity", "expired", "copy error", "wrong repository", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			p, d, run, now, _ := githubCredentialFixture(t)
			ctx := context.Background()
			source := p.config.GitHubTokens
			switch scenario {
			case "source error":
				p.config.GitHubTokens = credentialSourceFunc(func(context.Context, githubapp.RepositoryIdentity) (githubapp.InstallationToken, error) {
					return githubapp.InstallationToken{}, errors.New(githubTestSecret)
				})
			case "zero token":
				p.config.GitHubTokens = credentialSourceFunc(func(context.Context, githubapp.RepositoryIdentity) (githubapp.InstallationToken, error) {
					return githubapp.InstallationToken{}, nil
				})
			case "wrong identity":
				p.config.GitHubRepository, _ = githubapp.NewRepositoryIdentity(102, 202)
				p.config.GitHubTokens = credentialSourceFunc(func(ctx context.Context, _ githubapp.RepositoryIdentity) (githubapp.InstallationToken, error) {
					id, _ := githubapp.NewRepositoryIdentity(101, 202)
					return source.InstallationToken(ctx, id)
				})
			case "expired":
				if err := p.RefreshGitHubCredentials(ctx, run); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(2 * time.Hour)
				d.copies, d.execs = 0, 0
				p.config.GitHubTokens = credentialSourceFunc(func(context.Context, githubapp.RepositoryIdentity) (githubapp.InstallationToken, error) {
					return githubapp.InstallationToken{}, errors.New(githubTestSecret)
				})
			case "copy error":
				d.copyError = errors.New(githubTestSecret)
			case "wrong repository":
				run.RepositoryID++
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := p.RefreshGitHubCredentials(ctx, run)
			if err == nil || strings.Contains(err.Error(), githubTestSecret) {
				t.Fatalf("unsafe error = %v", err)
			}
			if d.execs != 0 || scenario != "copy error" && d.copies != 0 {
				t.Fatal("invalid source caused mutation")
			}
		})
	}
}

func TestGitHubCredentialImageRejectsOldRuntime(t *testing.T) {
	image := qualifiedImage()
	delete(image.Config.Labels, "ai.fern.runtime.spec")
	if qualifyImage(image, testImageID) == nil {
		t.Fatal("image without credential helper qualification accepted")
	}
}

func TestGitHubCredentialExpiredSourceAndCachedRuntimeAreRevalidated(t *testing.T) {
	p, d, run, now, calls := githubCredentialFixture(t)
	source := p.config.GitHubTokens
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	token, err := source.InstallationToken(ctx, p.config.GitHubRepository)
	if err != nil {
		t.Fatal(err)
	}
	p.config.GitHubTokens = credentialSourceFunc(func(context.Context, githubapp.RepositoryIdentity) (githubapp.InstallationToken, error) {
		return token, nil
	})
	if err := p.RefreshGitHubCredentials(ctx, run); err != nil {
		t.Fatal(err)
	}
	d.info.State.StartedAt = "2026-09-05T12:00:00.000000001Z"
	if err := p.RefreshGitHubCredentials(ctx, run); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("cached runtime was not revalidated: %v", err)
	}
	d.info.State.StartedAt = run.ObservedContainerStartedAt
	*now = now.Add(11 * time.Minute)
	if err := p.RefreshGitHubCredentials(ctx, run); err == nil {
		t.Fatal("expired source token accepted")
	}
	if d.copies != 1 || d.execs != 1 || *calls != 1 {
		t.Fatal("expired source token caused mutation")
	}
}

func TestGitHubCredentialChangedRuntimeBetweenStagingAndInstall(t *testing.T) {
	p, d, run, _, _ := githubCredentialFixture(t)
	d.inspectHook = func() {
		if d.copies != 0 {
			d.info.State.StartedAt = "2026-09-05T12:00:00.000000001Z"
		}
	}
	if err := p.RefreshGitHubCredentials(context.Background(), run); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v", err)
	}
	if d.copies != 1 || d.execs != 0 {
		t.Fatal("changed process received installation exec")
	}
}
