package taskenvdocker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/domain"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/store"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	runRootName      = "background-runs"
	hostKeyName      = "host.key"
	serverPort       = nat.Port("4096/tcp")
	workspaceTarget  = "/home/user/workspace"
	opencodeTarget   = "/home/user/.local/share/opencode"
	containerUser    = "1001:1001"
	passwordEnv      = "OPENCODE_SERVER_PASSWORD"
	usernameEnv      = "OPENCODE_SERVER_USERNAME"
	basicUsername    = "opencode"
	managedLabel     = "dev.fern.background-run.managed"
	workspaceLabel   = "dev.fern.background-run.workspace"
	taskLabel        = "dev.fern.background-run.task"
	imageLabel       = "dev.fern.background-run.image"
	cloneLabel       = "dev.fern.background-run.clone"
	volumeLabel      = "dev.fern.background-run.volume"
	containerLabel   = "dev.fern.background-run.container"
	endpointLabel    = "dev.fern.background-run.endpoint"
	baseLabel        = "dev.fern.background-run.base"
	repositoryLabel  = "dev.fern.background-run.repository"
	profileLabel     = "dev.fern.background-run.profile"
	sessionLabel     = "dev.fern.background-run.session"
	messageLabel     = "dev.fern.background-run.message"
	specLabel        = "dev.fern.background-run.spec"
	markerName       = "fern-background-run.json"
	passwordDomain   = "fern/background-run/basic-password/v1\x00"
	expectedSource   = "https://github.com/anomalyco/opencode"
	expectedRevision = "39fb919a054190498f6d5b7985bde231f93ad7a6"
	expectedVersion  = "0.0.0-source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	expectedProfile  = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	maxEvidenceBytes = 4096
	maxHealthBytes   = 4096
)

var (
	ErrIdentityMismatch = errors.New("background run resource identity mismatch")
	ErrQuarantined      = errors.New("background run resource quarantined")
	ErrProviderClosed   = errors.New("background run Docker provider is closed")
	// ErrRuntimeExited means the run's container already started and exited
	// before its runtime was recorded. It must not be restarted; callers move
	// the run to cleanup, which adopts the attested runtime.
	ErrRuntimeExited = errors.New("background run container exited before its runtime was recorded and may not be restarted")
)

// IdentityError means a pre-existing resource could not be proven to be the
// exact intended resource. Callers must quarantine it for operator review.
type IdentityError struct {
	Resource string
	Identity string
	Reason   string
}

func (e *IdentityError) Error() string {
	return fmt.Sprintf("%s %q is quarantined: %s", e.Resource, e.Identity, e.Reason)
}

func (e *IdentityError) Unwrap() error { return errors.Join(ErrIdentityMismatch, ErrQuarantined) }

// Every background container gets exactly these CPU and PID limits; they are
// fixed policy rather than configuration.
const (
	containerNanoCPUs int64 = 2_000_000_000 // 2 CPUs
	containerPIDs     int64 = 512
)

// Config contains server policy, not client-supplied run identities.
type Config struct {
	StateRoot string
	// RuntimeStorageRoot is an operator-provisioned XFS project directory. It
	// must not contain the durable task database. Empty permits cleanup only.
	RuntimeStorageRoot       string
	quotaCheck               func(string) (quotaIdentity, error) // hermetic package tests only
	Repository               string
	GitExecutable            string
	ImageReference           string
	ImageID                  string
	MemoryBytes              int64
	WallTimeout              time.Duration
	GitTimeout               time.Duration
	DockerTimeout            time.Duration
	HealthTimeout            time.Duration
	GitOutputBytes           int64
	SourceSizeAdmissionBytes int64
	CloneObservedLimitBytes  int64
	DiskFreeAdmissionBytes   int64
	LogMaxSize               string
	LogMaxFiles              int
	StopGrace                time.Duration
	// GitHubTokens is nil only for hermetic no-GitHub tests/setup. Production
	// supplies a repository-scoped App source and its exact configured identity.
	GitHubTokens             githubapp.InstallationTokenSource
	GitHubRepository         githubapp.RepositoryIdentity
	GitHubRepositoryFullName string
}

type dockerAPI interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (image.InspectResponse, error)
	VolumeCreate(context.Context, volume.CreateOptions) (volume.Volume, error)
	VolumeInspect(context.Context, string) (volume.Volume, error)
	VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(context.Context, string, bool) error
	ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error)
	ContainerInspect(context.Context, string) (container.InspectResponse, error)
	ContainerList(context.Context, container.ListOptions) ([]container.Summary, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerRemove(context.Context, string, container.RemoveOptions) error
}

// Provider is safe to reconstruct: all credentials and resource expectations
// derive from durable state and immutable run identity.
type Provider struct {
	config           Config
	docker           dockerAPI
	ownedCLI         *client.Client
	root             string
	hostKey          [32]byte
	imageEnv         map[string]string
	imageLabels      map[string]string
	http             *http.Client
	lifecycle        *providerLifecycle
	githubCredential githubCredentialCache
	githubNow        func() time.Time
}

type providerLifecycle struct {
	githubMu  sync.Mutex
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	closed    bool
}

// Observation is bounded canonical evidence suitable for store evidence
// columns. It never contains credentials or environment values.
type Observation struct {
	Evidence         string
	ContainerID      string
	ContainerStarted string
	RuntimeEpoch     int64
	RuntimeToken     string
	HostPort         int
	Endpoint         string
}

// RuntimeIdentity returns the exact process fence carried by this observation.
func (o Observation) RuntimeIdentity() RuntimeIdentity {
	return RuntimeIdentity{ContainerID: o.ContainerID, StartedAt: o.ContainerStarted, Token: o.RuntimeToken}
}

// RuntimeIdentity is the exact committed Docker process epoch. StartedAt is
// retained at Docker's full RFC3339Nano precision; Token is its canonical
// digest for compact comparison and evidence.
type RuntimeIdentity struct {
	ContainerID string
	StartedAt   string
	Token       string
}

// UsageObservation is bounded monitoring evidence, not a filesystem quota.
// Docker local-volume usage is intentionally unavailable here.
type UsageObservation struct {
	Evidence string
}

type evidence struct {
	Version   int    `json:"version"`
	Effect    string `json:"effect"`
	Identity  string `json:"identity"`
	Spec      string `json:"spec"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Container string `json:"container_id,omitempty"`
	Started   string `json:"started_at,omitempty"`
	Port      int    `json:"host_port,omitempty"`
	Runtime   string `json:"runtime_token,omitempty"`
	Bytes     int64  `json:"observed_bytes,omitempty"`
	Limit     int64  `json:"observed_limit_bytes,omitempty"`
}

// EnvironmentSHA256 identifies the disposable container environment without
// persisting its values in the task store. Environment injection is
// unsupported (the worker has unrestricted bridge egress), so every current
// run records EnvironmentSHA256(nil).
func EnvironmentSHA256(environment map[string]string) [sha256.Size]byte {
	if environment == nil {
		environment = map[string]string{}
	}
	encoded, _ := json.Marshal(environment)
	return sha256.Sum256(encoded)
}

// New validates the complete policy, qualifies the immutable local image, and
// atomically creates or loads the state-backed host key.
func New(ctx context.Context, config Config, api dockerAPI) (*Provider, error) {
	// A supplied Docker implementation is already a trusted attestation
	// boundary. Hermetic cross-package fakes may attest storage through that
	// same boundary; the production Docker client never implements this seam.
	if verifier, ok := api.(interface{ VerifyRuntimeStorage(string) error }); ok {
		config.quotaCheck = func(path string) (quotaIdentity, error) {
			return quotaIdentity{Device: 1, Project: 1, Blocks: 1, Inodes: 1}, verifier.VerifyRuntimeStorage(path)
		}
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	root, hostKey, err := prepareRoot(config.StateRoot)
	if err != nil {
		return nil, err
	}
	if config.RuntimeStorageRoot != "" {
		root, _, err = prepareRootWithKey(config.RuntimeStorageRoot, &hostKey)
		if err != nil {
			return nil, err
		}
	}
	var owned *client.Client
	if api == nil {
		owned, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, fmt.Errorf("create Docker client: %w", err)
		}
		api = owned
	}
	inspection, imageEnv, err := inspectQualifiedImage(ctx, api, config)
	if err != nil {
		if owned != nil {
			_ = owned.Close()
		}
		return nil, err
	}
	httpClient := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Provider{config: config, docker: api, ownedCLI: owned, root: root, hostKey: hostKey, imageEnv: imageEnv, imageLabels: maps.Clone(inspection.Config.Labels), http: httpClient, lifecycle: &providerLifecycle{}}, nil
}

func (p *Provider) Close() error {
	p.lifecycle.closeOnce.Do(func() {
		p.lifecycle.mu.Lock()
		p.lifecycle.closed = true
		p.lifecycle.mu.Unlock()
		if p.ownedCLI != nil {
			p.lifecycle.closeErr = p.ownedCLI.Close()
		}
	})
	return p.lifecycle.closeErr
}

// CommittedRuntime reconstructs the exact durable process identity without
// requiring the current execution configuration to match the run being cleaned.
func (p *Provider) CommittedRuntime(run store.BackgroundRun) (RuntimeIdentity, error) {
	if _, err := p.validateRunForCleanup(run); err != nil {
		return RuntimeIdentity{}, err
	}
	return committedRuntimeFromRun(run)
}

func committedRuntimeFromRun(run store.BackgroundRun) (RuntimeIdentity, error) {
	identity, err := domain.NewRuntime(run.ObservedContainerID, run.ObservedContainerStartedAt)
	if err != nil || identity.Epoch() != run.RuntimeEpoch {
		return RuntimeIdentity{}, errors.New("durable background runtime epoch is incomplete")
	}
	return runtimeFromIdentity(identity), nil
}

// OpenCodeClient derives Basic credentials in memory and returns only the
// profile-specific authenticated client. No capability or secret crosses the
// provider boundary.
func (p *Provider) OpenCodeClient(run store.BackgroundRun, runtime RuntimeIdentity, httpClient *http.Client) (*backgroundopencode.Client, error) {
	if _, err := p.validateRun(run); err != nil {
		return nil, err
	}
	if err := validateCommittedRuntime(runtime); err != nil || runtime.ContainerID != run.ObservedContainerID ||
		runtime.StartedAt != run.ObservedContainerStartedAt || run.HostPort < 1 || run.HostPort > 65535 {
		return nil, errors.New("exact committed background runtime is required")
	}
	return backgroundopencode.New(backgroundopencode.Config{
		Endpoint: "http://127.0.0.1:" + strconv.Itoa(run.HostPort), Username: basicUsername,
		Password: p.password(run), HTTPClient: httpClient,
	})
}

// BackgroundRouteTarget derives the exact endpoint and an authenticated
// transport without exposing the run password outside the provider.
func (p *Provider) BackgroundRouteTarget(run store.BackgroundRun, runtime RuntimeIdentity) (backgroundroute.Target, error) {
	transport, err := p.newRouteTransport(run, runtime)
	if err != nil {
		return backgroundroute.Target{}, err
	}
	return backgroundroute.NewTarget("http://"+transport.endpoint, transport)
}

func (p *Provider) newRouteTransport(run store.BackgroundRun, runtime RuntimeIdentity) (*routeTransport, error) {
	digest, err := p.validateRun(run)
	if err != nil {
		return nil, err
	}
	if err := validateCommittedRuntime(runtime); err != nil || runtime.ContainerID != run.ObservedContainerID ||
		runtime.StartedAt != run.ObservedContainerStartedAt || run.HostPort < 1 || run.HostPort > 65535 {
		return nil, errors.New("exact committed background runtime is required")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("standard background route transport is unavailable")
	}
	base = base.Clone()
	endpoint := "127.0.0.1:" + strconv.Itoa(run.HostPort)
	dial := base.DialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: p.config.DockerTimeout, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	transport := &routeTransport{
		provider: p, run: run, digest: digest, runtime: runtime, hostPort: run.HostPort, endpoint: endpoint, dial: dial,
		username: basicUsername, password: p.password(run),
	}
	base.Proxy = nil
	base.DialContext = transport.dialContext
	transport.base = base
	return transport, nil
}

// routeTransport forwards proxied requests only over connections attested to
// the exact routed runtime. A TCP connection to the published loopback port is
// bound to the container process that held the port when it was dialed and
// cannot migrate to a replacement, so attesting at dial time keeps every
// request on the exact runtime without a Docker inspect per request.
type routeTransport struct {
	base               http.RoundTripper
	provider           *Provider
	run                store.BackgroundRun
	digest             string
	runtime            RuntimeIdentity
	hostPort           int
	endpoint           string
	dial               func(context.Context, string, string) (net.Conn, error)
	username, password string
}

func (transport *routeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forward := request.Clone(request.Context())
	forward.Header = request.Header.Clone()
	forward.Header.Del("Authorization")
	forward.Header.Del("Cookie")
	forward.SetBasicAuth(transport.username, transport.password)
	return transport.base.RoundTrip(forward)
}

func (transport *routeTransport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" || address != transport.endpoint {
		return nil, errors.New("background route dial target is not exact")
	}
	connection, err := transport.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if err := transport.attest(ctx); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

func (transport *routeTransport) attest(ctx context.Context) error {
	operation, cancel := context.WithTimeout(ctx, transport.provider.config.DockerTimeout)
	defer cancel()
	info, err := transport.provider.docker.ContainerInspect(operation, transport.run.ContainerIdentity)
	if err != nil {
		return err
	}
	if info.ID != transport.runtime.ContainerID {
		return &IdentityError{Resource: "container", Identity: transport.run.ContainerIdentity, Reason: "named container ID differs from routed runtime"}
	}
	if err := requireRuntime(info, transport.runtime); err != nil {
		return err
	}
	if err := transport.provider.attestContainer(transport.run, transport.digest, info, true); err != nil {
		return &IdentityError{Resource: "container", Identity: transport.run.ContainerIdentity, Reason: err.Error()}
	}
	port, err := hostPort(info)
	if err != nil {
		return err
	}
	if port != transport.hostPort {
		return &IdentityError{Resource: "endpoint", Identity: transport.run.EndpointIdentity, Reason: "published port differs from routed runtime"}
	}
	return nil
}

func validateConfig(c Config) error {
	if c.GitHubTokens != nil {
		identity, err := githubapp.NewRepositoryIdentity(c.GitHubRepository.InstallationID(), c.GitHubRepository.RepositoryID())
		if err != nil || identity != c.GitHubRepository || domain.ValidateOwnerRepo(c.GitHubRepositoryFullName) != nil {
			return errors.New("exact GitHub App repository identity is required")
		}
	}
	for name, value := range map[string]string{"state root": c.StateRoot, "repository": c.Repository, "Git executable": c.GitExecutable} {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("valid absolute %s is required", name)
		}
	}
	if !validImageID(c.ImageID) {
		return errors.New("qualified immutable background image ID is required")
	}
	if c.ImageReference == "" || strings.TrimSpace(c.ImageReference) != c.ImageReference || len(c.ImageReference) > 512 {
		return errors.New("qualified background image reference is required")
	}
	// The limits are Fern's own constants (cmd/fern); only reject unset ones.
	for _, limit := range []struct {
		name string
		ok   bool
	}{
		{"MemoryBytes", c.MemoryBytes > 0},
		{"WallTimeout", c.WallTimeout > 0},
		{"GitTimeout", c.GitTimeout > 0},
		{"DockerTimeout", c.DockerTimeout > 0},
		{"HealthTimeout", c.HealthTimeout > 0},
		{"GitOutputBytes", c.GitOutputBytes > 0},
		{"SourceSizeAdmissionBytes", c.SourceSizeAdmissionBytes > 0},
		{"CloneObservedLimitBytes", c.CloneObservedLimitBytes > 0},
		{"DiskFreeAdmissionBytes", c.DiskFreeAdmissionBytes > 0},
		{"LogMaxFiles", c.LogMaxFiles > 0},
		{"StopGrace", c.StopGrace >= 0},
	} {
		if !limit.ok {
			return fmt.Errorf("background run Config.%s is unset or negative", limit.name)
		}
	}
	if _, err := parseLogSize(c.LogMaxSize); err != nil {
		return err
	}
	for name, path := range map[string]string{"state root": c.StateRoot, "repository": c.Repository, "Git executable": c.GitExecutable} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must exist without symlinks", name)
		}
		if name == "Git executable" && (!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0) {
			return errors.New("Git executable must be an exact executable regular file")
		}
		if name != "Git executable" && !info.IsDir() {
			return fmt.Errorf("%s must be a directory", name)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			return fmt.Errorf("%s must be an exact path without symlink components", name)
		}
	}
	return nil
}

func inspectQualifiedImage(ctx context.Context, api dockerAPI, config Config) (image.InspectResponse, map[string]string, error) {
	operation, cancel := context.WithTimeout(ctx, config.DockerTimeout)
	defer cancel()
	inspection, err := api.ImageInspect(operation, config.ImageReference)
	if err != nil {
		return image.InspectResponse{}, nil, fmt.Errorf("inspect qualified background image: %w", err)
	}
	if err := qualifyImage(inspection, config.ImageID); err != nil {
		return image.InspectResponse{}, nil, err
	}
	imageEnv, err := parseEnvironment(inspection.Config.Env)
	if err != nil {
		return image.InspectResponse{}, nil, fmt.Errorf("qualified image environment: %w", err)
	}
	return inspection, imageEnv, nil
}

func qualifyImage(got image.InspectResponse, want string) error {
	if got.Config == nil || got.Config.Labels["ai.fern.runtime.spec"] != strconv.Itoa(domain.ResourceSpecVersion) {
		return errors.New("background image runtime credential profile is not qualified")
	}
	if got.ID != want || got.Config == nil || got.Config.User != containerUser || len(got.Config.Entrypoint) != 0 ||
		!slices.Equal(got.Config.Cmd, []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"}) ||
		len(got.Config.ExposedPorts) != 1 || !maps.Equal(got.Config.Volumes, map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}) {
		return errors.New("background image does not match qualified immutable source profile")
	}
	if _, ok := got.Config.ExposedPorts[serverPort]; !ok || got.Config.Labels["org.opencontainers.image.source"] != expectedSource || got.Config.Labels["org.opencontainers.image.revision"] != expectedRevision || got.Config.Labels["org.opencontainers.image.version"] != expectedVersion || got.Config.Labels["ai.fern.opencode.profile"] != expectedProfile {
		return errors.New("background image source identity is not qualified")
	}
	for _, entry := range got.Config.Env {
		key, _, _ := strings.Cut(entry, "=")
		if key == passwordEnv || key == usernameEnv {
			return errors.New("background image contains baked server credentials")
		}
	}
	return nil
}

func (p *Provider) validateRun(run store.BackgroundRun) (string, error) {
	if err := p.admitStorage(); err != nil {
		return "", err
	}
	digest, err := p.validateRunForCleanup(run)
	if err != nil {
		return "", err
	}
	if run.ImageIdentity != p.config.ImageID || run.EnvironmentSHA256 != EnvironmentSHA256(nil) {
		return "", errors.New("background run execution configuration differs from immutable intent")
	}
	return digest, nil
}

func (p *Provider) validateRunForCleanup(run store.BackgroundRun) (string, error) {
	// The run row is Fern's own durable record; only the derived resource names
	// are re-checked because they become host paths and Docker object names.
	if !validImageID(run.ImageIdentity) || run.ResourceSpecVersion != domain.ResourceSpecVersion || run.Profile != store.BackgroundRunSourceProfile {
		return "", errors.New("invalid immutable background run tuple")
	}
	if !domain.NewResources(run.RunID).Matches(run.CloneIdentity, run.VolumeIdentity, run.ContainerIdentity, run.EndpointIdentity) {
		return "", errors.New("noncanonical background run resource identity")
	}
	return p.specDigest(run)
}

func (p *Provider) specDigest(run store.BackgroundRun) (string, error) {
	data, err := json.Marshal(struct {
		Version                                                                                int `json:"version"`
		Workspace, Task                                                                        string
		Image, Clone, Volume, Container, Endpoint, Base, Repository, Profile, Session, Message string
		EnvironmentSHA256                                                                      string
	}{
		Version: domain.ResourceSpecVersion, Workspace: string(run.WorkspaceID), Task: string(run.RunID),
		Image: run.ImageIdentity, Clone: run.CloneIdentity, Volume: run.VolumeIdentity,
		Container: run.ContainerIdentity, Endpoint: run.EndpointIdentity, Base: string(run.BaseOID), Repository: run.RepositoryRemote,
		Profile: run.Profile, Session: string(run.OpenCodeSessionID), Message: string(run.OpenCodeMessageID),
		EnvironmentSHA256: hex.EncodeToString(run.EnvironmentSHA256[:]),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (p *Provider) password(run store.BackgroundRun) string {
	mac := hmac.New(sha256.New, p.hostKey[:])
	_, _ = mac.Write([]byte(passwordDomain))
	for _, value := range []string{string(run.WorkspaceID), string(run.RunID), run.ImageIdentity} {
		_, _ = mac.Write([]byte(strconv.Itoa(len(value))))
		_, _ = mac.Write([]byte{':'})
		_, _ = mac.Write([]byte(value))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (p *Provider) labels(run store.BackgroundRun, digest string) map[string]string {
	return map[string]string{
		managedLabel: "true", workspaceLabel: string(run.WorkspaceID), taskLabel: string(run.RunID),
		imageLabel: run.ImageIdentity, cloneLabel: run.CloneIdentity,
		volumeLabel: run.VolumeIdentity, containerLabel: run.ContainerIdentity, endpointLabel: run.EndpointIdentity,
		baseLabel: string(run.BaseOID), repositoryLabel: run.RepositoryRemote, profileLabel: run.Profile,
		sessionLabel: string(run.OpenCodeSessionID), messageLabel: string(run.OpenCodeMessageID), specLabel: digest,
	}
}

func (p *Provider) containerLabels(run store.BackgroundRun, digest string) map[string]string {
	labels := make(map[string]string, len(p.imageLabels)+14)
	maps.Copy(labels, p.imageLabels)
	maps.Copy(labels, p.labels(run, digest))
	return labels
}

func makeEvidence(value evidence) (string, error) {
	value.Version = 1
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) > maxEvidenceBytes {
		return "", errors.New("background run evidence exceeds bound")
	}
	return string(data), nil
}

func validImageID(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil && strings.ToLower(value) == value
}

func validEnvKey(value string) bool {
	if value == "" || !((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z') || value[0] == '_') {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func parseEnvironment(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !validEnvKey(key) {
			return nil, errors.New("invalid environment entry")
		}
		if _, exists := result[key]; exists {
			return nil, errors.New("duplicate environment entry")
		}
		result[key] = value
	}
	return result, nil
}

func parseLogSize(value string) (int64, error) {
	if len(value) < 2 {
		return 0, errors.New("valid Docker log max-size is required")
	}
	var multiplier int64
	switch value[len(value)-1] {
	case 'k':
		multiplier = 1 << 10
	case 'm':
		multiplier = 1 << 20
	case 'g':
		multiplier = 1 << 30
	default:
		return 0, errors.New("Docker log max-size must use k, m, or g")
	}
	number, err := strconv.ParseInt(value[:len(value)-1], 10, 64)
	if err != nil || number < 1 || number > (1<<30)/multiplier {
		return 0, errors.New("Docker log max-size is outside the 1 GiB bound")
	}
	return number * multiplier, nil
}
