// Package taskenvdocker implements the disposable Docker environment used by
// one serial Fern Background Run. It deliberately does not schedule runs or
// mutate taskstore lifecycle state.
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	runRootName       = "background-runs"
	hostKeyName       = "host.key"
	serverPort        = nat.Port("4096/tcp")
	workspaceTarget   = "/home/user/workspace"
	opencodeTarget    = "/home/user/.local/share/opencode"
	containerUser     = "1001:1001"
	passwordEnv       = "OPENCODE_SERVER_PASSWORD"
	usernameEnv       = "OPENCODE_SERVER_USERNAME"
	managedLabel      = "dev.fern.background-run.managed"
	workspaceLabel    = "dev.fern.background-run.workspace"
	taskLabel         = "dev.fern.background-run.task"
	attemptLabel      = "dev.fern.background-run.attempt"
	generationLabel   = "dev.fern.background-run.generation"
	imageLabel        = "dev.fern.background-run.image"
	cloneLabel        = "dev.fern.background-run.clone"
	volumeLabel       = "dev.fern.background-run.volume"
	containerLabel    = "dev.fern.background-run.container"
	endpointLabel     = "dev.fern.background-run.endpoint"
	baseLabel         = "dev.fern.background-run.base"
	repositoryLabel   = "dev.fern.background-run.repository"
	profileLabel      = "dev.fern.background-run.profile"
	sessionLabel      = "dev.fern.background-run.session"
	messageLabel      = "dev.fern.background-run.message"
	specLabel         = "dev.fern.background-run.spec"
	markerName        = "fern-background-run.json"
	passwordDomain    = "fern/background-run/basic-password/v1\x00"
	environmentDomain = "fern/background-run/environment/v1\x00"
	expectedSource    = "https://github.com/anomalyco/opencode"
	expectedRevision  = "39fb919a054190498f6d5b7985bde231f93ad7a6"
	expectedVersion   = "0.0.0-source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	expectedProfile   = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	maxEvidenceBytes  = 4096
	maxHealthBytes    = 4096
)

var (
	ErrIdentityMismatch = errors.New("background run resource identity mismatch")
	ErrQuarantined      = errors.New("background run resource quarantined")
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

// Config contains server policy, not client-supplied run identities.
type Config struct {
	StateRoot                string
	Repository               string
	GitExecutable            string
	ImageReference           string
	ImageID                  string
	MemoryBytes              int64
	NanoCPUs                 int64
	PIDs                     int64
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
	BasicUsername            string
	Environment              map[string]string
	HTTPClient               *http.Client
}

type dockerAPI interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (image.InspectResponse, error)
	VolumeCreate(context.Context, volume.CreateOptions) (volume.Volume, error)
	VolumeInspect(context.Context, string) (volume.Volume, error)
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
	config      Config
	docker      dockerAPI
	ownedCLI    *client.Client
	root        string
	hostKey     [32]byte
	imageEnv    map[string]string
	imageLabels map[string]string
	http        *http.Client
}

// Observation is bounded canonical evidence suitable for taskstore evidence
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

// CleanupAuthority is one of three explicit lifecycle proofs: NeverCreated,
// an exact created-but-never-started ID, or a full committed process epoch.
// Its zero value is invalid.
type CleanupAuthority struct {
	NeverCreated bool
	ContainerID  string
	StartedAt    string
	Token        string
}

func NeverCreatedAuthority() CleanupAuthority {
	return CleanupAuthority{NeverCreated: true}
}

func CreatedContainerAuthority(containerID string) CleanupAuthority {
	return CleanupAuthority{ContainerID: containerID}
}

func RuntimeCleanupAuthority(runtime RuntimeIdentity) CleanupAuthority {
	return CleanupAuthority{ContainerID: runtime.ContainerID, StartedAt: runtime.StartedAt, Token: runtime.Token}
}

func (a CleanupAuthority) runtimeIdentity() RuntimeIdentity {
	return RuntimeIdentity{ContainerID: a.ContainerID, StartedAt: a.StartedAt, Token: a.Token}
}

// UsageObservation is bounded monitoring evidence, not a filesystem quota.
// Docker local-volume usage is intentionally unavailable here.
type UsageObservation struct {
	Evidence             string
	CloneBytes           int64
	ObservedLimitBytes   int64
	VolumeBytesAvailable bool
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

// EnvironmentSHA256 identifies the exact explicitly configured disposable
// environment without persisting its values in the task store.
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
	if config.BasicUsername == "" {
		config.BasicUsername = "opencode"
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	root, hostKey, err := prepareRoot(config.StateRoot)
	if err != nil {
		return nil, err
	}
	var owned *client.Client
	if api == nil {
		owned, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, fmt.Errorf("create Docker client: %w", err)
		}
		api = owned
	}
	operation, cancel := context.WithTimeout(ctx, config.DockerTimeout)
	defer cancel()
	inspection, err := api.ImageInspect(operation, config.ImageReference)
	if err != nil {
		if owned != nil {
			_ = owned.Close()
		}
		return nil, fmt.Errorf("inspect qualified background image: %w", err)
	}
	if err := qualifyImage(inspection, config.ImageID); err != nil {
		if owned != nil {
			_ = owned.Close()
		}
		return nil, err
	}
	imageEnv, err := parseEnvironment(inspection.Config.Env)
	if err != nil {
		if owned != nil {
			_ = owned.Close()
		}
		return nil, fmt.Errorf("qualified image environment: %w", err)
	}
	httpClient := &http.Client{}
	if config.HTTPClient != nil {
		*httpClient = *config.HTTPClient
	}
	if httpClient.Timeout <= 0 || httpClient.Timeout > 2*time.Second {
		httpClient.Timeout = 2 * time.Second
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Provider{config: cloneConfig(config), docker: api, ownedCLI: owned, root: root, hostKey: hostKey, imageEnv: imageEnv, imageLabels: cloneMap(inspection.Config.Labels), http: httpClient}, nil
}

func (p *Provider) Close() error {
	if p.ownedCLI != nil {
		return p.ownedCLI.Close()
	}
	return nil
}

// CommittedRuntime reconstructs the provider's exact process identity from the
// durable schema-8 observation without exposing or persisting credentials.
func (p *Provider) CommittedRuntime(run taskstore.BackgroundRun) (RuntimeIdentity, error) {
	if _, err := p.validateRun(run); err != nil {
		return RuntimeIdentity{}, err
	}
	started, err := time.Parse(time.RFC3339Nano, run.ObservedContainerStartedAt)
	if err != nil || started.UnixNano() != run.RuntimeEpoch {
		return RuntimeIdentity{}, errors.New("durable background runtime epoch is incomplete")
	}
	runtime := RuntimeIdentity{ContainerID: run.ObservedContainerID, StartedAt: run.ObservedContainerStartedAt,
		Token: runtimeToken(run.ObservedContainerID, run.ObservedContainerStartedAt)}
	if err := validateCommittedRuntime(runtime); err != nil {
		return RuntimeIdentity{}, err
	}
	return runtime, nil
}

// OpenCodeClient derives Basic credentials in memory and returns only the
// profile-specific authenticated client. No capability or secret crosses the
// provider boundary.
func (p *Provider) OpenCodeClient(run taskstore.BackgroundRun, runtime RuntimeIdentity, httpClient *http.Client) (*backgroundopencode.Client, error) {
	if _, err := p.validateRun(run); err != nil {
		return nil, err
	}
	if err := validateCommittedRuntime(runtime); err != nil || runtime.ContainerID != run.ObservedContainerID ||
		runtime.StartedAt != run.ObservedContainerStartedAt || run.HostPort < 1 || run.HostPort > 65535 {
		return nil, errors.New("exact committed background runtime is required")
	}
	return backgroundopencode.New(backgroundopencode.Config{
		Endpoint: "http://127.0.0.1:" + strconv.Itoa(run.HostPort), Username: p.config.BasicUsername,
		Password: p.password(run), HTTPClient: httpClient,
	})
}

func cloneConfig(config Config) Config {
	config.Environment = cloneMap(config.Environment)
	return config
}

func validateConfig(c Config) error {
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
	if c.MemoryBytes < 64<<20 || c.MemoryBytes > 1<<40 || c.NanoCPUs != 2_000_000_000 || c.PIDs != 512 || c.WallTimeout <= 0 || c.WallTimeout > 7*24*time.Hour || c.GitTimeout <= 0 || c.GitTimeout > 10*time.Minute || c.DockerTimeout <= 0 || c.DockerTimeout > 10*time.Minute || c.HealthTimeout <= 0 || c.HealthTimeout > 10*time.Minute || c.GitOutputBytes < 1024 || c.GitOutputBytes > 16<<20 || c.SourceSizeAdmissionBytes <= 0 || c.SourceSizeAdmissionBytes > 1<<40 || c.CloneObservedLimitBytes < c.SourceSizeAdmissionBytes || c.CloneObservedLimitBytes > 1<<40 || c.DiskFreeAdmissionBytes < c.CloneObservedLimitBytes || c.DiskFreeAdmissionBytes > 1<<40 || c.LogMaxSize == "" || c.LogMaxFiles < 1 || c.LogMaxFiles > 100 || c.StopGrace < 0 || c.StopGrace > time.Minute {
		return errors.New("valid bounded memory, 2 CPU, 512 PID, wall, output, disk, log, and stop limits are required")
	}
	if c.GitTimeout > c.WallTimeout || c.DockerTimeout > c.WallTimeout || c.HealthTimeout > c.WallTimeout {
		return errors.New("operation timeout exceeds run wall limit")
	}
	if !validEnvToken(c.BasicUsername) {
		return errors.New("valid Basic username is required")
	}
	if _, err := parseLogSize(c.LogMaxSize); err != nil {
		return err
	}
	totalEnvironment := 0
	for key, value := range c.Environment {
		totalEnvironment += len(key) + len(value)
		if !validEnvKey(key) || len(key) > 128 || len(value) > 64<<10 || strings.IndexByte(value, 0) >= 0 || key == "OPENCODE_PASSWORD" || key == passwordEnv || key == usernameEnv {
			return fmt.Errorf("invalid or reserved environment key %q", key)
		}
	}
	if totalEnvironment > 1<<20 {
		return errors.New("configured environment exceeds 1 MiB")
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

func qualifyImage(got image.InspectResponse, want string) error {
	if got.ID != want || got.Config == nil || got.Config.User != containerUser || len(got.Config.Entrypoint) != 0 ||
		!slices.Equal(got.Config.Cmd, []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"}) ||
		len(got.Config.ExposedPorts) != 1 || !equalMap(got.Config.Volumes, map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}) {
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

func (p *Provider) validateRun(run taskstore.BackgroundRun) (string, error) {
	digest, err := p.validateRunForCleanup(run)
	if err != nil {
		return "", err
	}
	if run.ResourceSpecVersion != 9 || run.ImageIdentity != p.config.ImageID || run.EnvironmentSHA256 != EnvironmentSHA256(p.config.Environment) {
		return "", errors.New("background run execution configuration differs from immutable intent")
	}
	return digest, nil
}

func (p *Provider) validateRunForCleanup(run taskstore.BackgroundRun) (string, error) {
	if _, err := task.ParseWorkspaceID(string(run.WorkspaceID)); err != nil {
		return "", err
	}
	if _, err := task.ParseTaskID(string(run.TaskID)); err != nil {
		return "", err
	}
	if _, err := task.ParseAttemptID(string(run.AttemptID)); err != nil || run.Generation <= 0 || !validImageID(run.ImageIdentity) ||
		run.EnvironmentSHA256 == ([sha256.Size]byte{}) || (run.ResourceSpecVersion != 8 && run.ResourceSpecVersion != 9) || run.Profile != taskstore.BackgroundRunSourceProfile {
		return "", errors.New("invalid immutable background run tuple")
	}
	if _, err := task.ParseGitOID(string(run.BaseOID)); err != nil {
		return "", err
	}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(run.TaskID), "tsk_"), "-", "")
	generation := strconv.FormatInt(run.Generation, 10)
	if run.CloneIdentity != "run-"+compact+"-g"+generation+"-clone" || run.VolumeIdentity != "fern-run-"+compact+"-g"+generation+"-opencode" || run.ContainerIdentity != "fern-run-"+compact+"-g"+generation || run.EndpointIdentity != "run-"+compact+"-g"+generation+"-endpoint" {
		return "", errors.New("noncanonical background run resource identity")
	}
	if strings.ContainsAny(run.CloneIdentity+run.VolumeIdentity+run.ContainerIdentity, `/\\`) || !canonicalRemote(run.RepositoryRemote) || run.OpenCodeSessionID == "" || run.OpenCodeMessageID == "" {
		return "", errors.New("incomplete background run identity")
	}
	return p.specDigest(run)
}

func (p *Provider) cleanupDigest(run taskstore.BackgroundRun) (string, error) {
	digest, err := p.validateRunForCleanup(run)
	if err != nil || run.ResourceSpecVersion != 8 {
		return digest, err
	}
	if _, err := os.Lstat(p.cloneMarkerPath(run)); errors.Is(err, os.ErrNotExist) {
		return digest, nil
	} else if err != nil {
		return "", err
	}
	snapshot, err := p.readCloneMarkerSnapshotUnbound(run)
	if err != nil || !validSpecDigest(snapshot.marker.Spec) {
		return "", errors.Join(errors.New("schema-8 clone authority has an invalid resource digest"), err)
	}
	return snapshot.marker.Spec, nil
}

func canonicalRemote(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Host, "github.com") && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" && parsed.Path != "/" && !strings.HasSuffix(parsed.Path, "/") && !strings.HasSuffix(strings.ToLower(parsed.Path), ".git") && value == "https://"+strings.ToLower(parsed.Host)+parsed.EscapedPath()
}

func (p *Provider) specDigest(run taskstore.BackgroundRun) (string, error) {
	if run.ResourceSpecVersion == 8 {
		return p.legacySpecDigest(run)
	}
	data, err := json.Marshal(struct {
		Version                                                                                int `json:"version"`
		Workspace, Task, Attempt                                                               string
		Generation                                                                             int64
		Image, Clone, Volume, Container, Endpoint, Base, Repository, Profile, Session, Message string
		EnvironmentSHA256                                                                      string
	}{
		Version: 9, Workspace: string(run.WorkspaceID), Task: string(run.TaskID), Attempt: string(run.AttemptID), Generation: run.Generation,
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

func (p *Provider) legacySpecDigest(run taskstore.BackgroundRun) (string, error) {
	environment := make([]string, 0, len(p.config.Environment))
	for key, value := range p.config.Environment {
		environment = append(environment, key+"="+value)
	}
	slices.Sort(environment)
	environmentMAC := hmac.New(sha256.New, p.hostKey[:])
	_, _ = environmentMAC.Write([]byte(environmentDomain))
	for _, entry := range environment {
		_, _ = environmentMAC.Write([]byte(strconv.Itoa(len(entry))))
		_, _ = environmentMAC.Write([]byte{':'})
		_, _ = environmentMAC.Write([]byte(entry))
	}
	data, err := json.Marshal(struct {
		Version                                                                                   int `json:"version"`
		Workspace, Task, Attempt                                                                  string
		Generation                                                                                int64
		ImageReference, Image, Clone, Volume, Container, Endpoint, Base, Repository, Profile      string
		Session, Message, BasicUsername                                                           string
		Memory, CPUs, PIDs, SourceSizeAdmission, CloneObservedLimit, DiskFreeAdmission, GitOutput int64
		WallTimeout, GitTimeout, DockerTimeout, HealthTimeout, StopGrace                          int64
		LogSize                                                                                   string
		LogFiles                                                                                  int
		EnvironmentMAC                                                                            string
	}{
		Version: 8, Workspace: string(run.WorkspaceID), Task: string(run.TaskID), Attempt: string(run.AttemptID), Generation: run.Generation,
		ImageReference: p.config.ImageReference, Image: run.ImageIdentity, Clone: run.CloneIdentity, Volume: run.VolumeIdentity,
		Container: run.ContainerIdentity, Endpoint: run.EndpointIdentity, Base: string(run.BaseOID), Repository: run.RepositoryRemote,
		Profile: run.Profile, Session: string(run.OpenCodeSessionID), Message: string(run.OpenCodeMessageID), BasicUsername: p.config.BasicUsername,
		Memory: p.config.MemoryBytes, CPUs: p.config.NanoCPUs, PIDs: p.config.PIDs, SourceSizeAdmission: p.config.SourceSizeAdmissionBytes,
		CloneObservedLimit: p.config.CloneObservedLimitBytes, DiskFreeAdmission: p.config.DiskFreeAdmissionBytes,
		GitOutput: p.config.GitOutputBytes, WallTimeout: int64(p.config.WallTimeout),
		GitTimeout: int64(p.config.GitTimeout), DockerTimeout: int64(p.config.DockerTimeout), HealthTimeout: int64(p.config.HealthTimeout),
		StopGrace: int64(p.config.StopGrace), LogSize: p.config.LogMaxSize, LogFiles: p.config.LogMaxFiles,
		EnvironmentMAC: hex.EncodeToString(environmentMAC.Sum(nil)),
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (p *Provider) password(run taskstore.BackgroundRun) string {
	mac := hmac.New(sha256.New, p.hostKey[:])
	_, _ = mac.Write([]byte(passwordDomain))
	for _, value := range []string{string(run.WorkspaceID), string(run.TaskID), string(run.AttemptID), strconv.FormatInt(run.Generation, 10), run.ImageIdentity} {
		_, _ = mac.Write([]byte(strconv.Itoa(len(value))))
		_, _ = mac.Write([]byte{':'})
		_, _ = mac.Write([]byte(value))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (p *Provider) labels(run taskstore.BackgroundRun, digest string) map[string]string {
	return map[string]string{
		managedLabel: "true", workspaceLabel: string(run.WorkspaceID), taskLabel: string(run.TaskID), attemptLabel: string(run.AttemptID),
		generationLabel: strconv.FormatInt(run.Generation, 10), imageLabel: run.ImageIdentity, cloneLabel: run.CloneIdentity,
		volumeLabel: run.VolumeIdentity, containerLabel: run.ContainerIdentity, endpointLabel: run.EndpointIdentity,
		baseLabel: string(run.BaseOID), repositoryLabel: run.RepositoryRemote, profileLabel: run.Profile,
		sessionLabel: string(run.OpenCodeSessionID), messageLabel: string(run.OpenCodeMessageID), specLabel: digest,
	}
}

func (p *Provider) containerLabels(run taskstore.BackgroundRun, digest string) map[string]string {
	labels := cloneMap(p.imageLabels)
	for key, value := range p.labels(run, digest) {
		labels[key] = value
	}
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

func operationContext(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, limit)
}

func validImageID(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil && strings.ToLower(value) == value
}

func validSpecDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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

func validEnvToken(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, ":\r\n\x00")
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

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func parseLogSize(value string) (int64, error) {
	if len(value) < 2 {
		return 0, errors.New("valid Docker log max-size is required")
	}
	multiplier := int64(1)
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
