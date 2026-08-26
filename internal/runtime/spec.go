package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
)

func sortedEnv(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for key, value := range env {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

func sortedEnvKeys(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for key := range env {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

type fingerprintValue struct {
	Version     int
	Name        string
	Image       string
	RepoPath    string
	MemoryBytes int64
	Env         []string
	Init        bool
	Port        string
	DataVolume  string
	WorkspaceGH bool   `json:",omitempty"`
	GHVolume    string `json:",omitempty"`
}

// specFingerprint attests the workspace definition stored in a container
// label. Environment contributes KEYS ONLY: secret values therefore never
// land in container labels or `docker inspect` output. Actual environment
// values are instead attested against the live container config by
// verifyActualSpec on every resume.
func specFingerprint(spec Spec) (string, error) {
	return fingerprint(fingerprintValue{
		Version:     1,
		Name:        spec.Name,
		Image:       spec.Image,
		RepoPath:    spec.RepoPath,
		MemoryBytes: spec.MemoryBytes,
		Env:         sortedEnvKeys(specEnvironment(spec)),
		Init:        true,
		Port:        workspacePort,
		DataVolume:  specDataVolumeName(spec),
		WorkspaceGH: spec.WorkspaceGH,
		GHVolume:    specGHVolumeName(spec),
	})
}

func fingerprint(value fingerprintValue) (string, error) {
	sort.Strings(value.Env)
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint workspace spec: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func specEnvironment(spec Spec) map[string]string {
	if !spec.WorkspaceGH {
		return spec.Env
	}
	env := make(map[string]string, len(spec.Env)+1)
	for key, value := range spec.Env {
		env[key] = value
	}
	env[githubConfigEnv] = githubConfigDir
	return env
}

func specMounts(spec Spec) []mount.Mount {
	mounts := []mount.Mount{
		{Type: mount.TypeBind, Source: spec.RepoPath, Target: repoMountTarget},
		{Type: mount.TypeVolume, Source: specDataVolumeName(spec), Target: dataMountTarget},
	}
	if spec.WorkspaceGH {
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: specGHVolumeName(spec), Target: githubConfigDir})
	}
	return mounts
}

// observedMount tallies actual container mounts keyed by their destination so
// duplicated destinations cannot masquerade as a single expected mount.
type observedMount struct {
	count  int
	source string
	name   string
}

// verifyActualSpec compares the live container configuration against spec.
// Expected mounts are derived from specMounts keyed by destination; every
// mismatch fails closed with ErrSpecDrift.
func verifyActualSpec(info container.InspectResponse, spec Spec) error {
	if info.Config == nil || info.HostConfig == nil {
		return fmt.Errorf("%w: Docker returned incomplete workspace configuration", ErrSpecDrift)
	}
	if info.Config.Image != spec.Image || info.Config.User != workspaceUser || info.HostConfig.Memory != spec.MemoryBytes || info.HostConfig.NanoCPUs != workspaceNanoCPUs || info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != workspacePIDs || info.HostConfig.Init == nil || !*info.HostConfig.Init || !info.HostConfig.RestartPolicy.IsNone() {
		return fmt.Errorf("%w: Docker image, memory, CPU, PID, init, or restart setting was modified; run 'fern down' to recreate", ErrSpecDrift)
	}
	if info.HostConfig.Privileged || info.HostConfig.ReadonlyRootfs || len(info.HostConfig.CapAdd) != 0 || len(info.HostConfig.CapDrop) != 1 || info.HostConfig.CapDrop[0] != "ALL" || len(info.HostConfig.Devices) != 0 || len(info.HostConfig.DeviceRequests) != 0 || len(info.HostConfig.SecurityOpt) != 1 || info.HostConfig.SecurityOpt[0] != "no-new-privileges" {
		return fmt.Errorf("%w: Docker privilege, capability, device, or security settings were modified; run 'fern down' to recreate", ErrSpecDrift)
	}
	bindings := info.HostConfig.PortBindings[nat.Port(workspacePort)]
	if _, ok := info.Config.ExposedPorts[nat.Port(workspacePort)]; !ok || len(bindings) != 1 || !isLoopbackBinding(bindings[0].HostIP) {
		return fmt.Errorf("%w: OpenCode port configuration was modified", ErrSpecDrift)
	}
	actualEnv := make(map[string]string, len(info.Config.Env))
	for _, entry := range info.Config.Env {
		key, value, _ := strings.Cut(entry, "=")
		actualEnv[key] = value
	}
	for key, value := range specEnvironment(spec) {
		actual, exists := actualEnv[key]
		if !exists || actual != value {
			return fmt.Errorf("%w: environment %s was modified", ErrSpecDrift, key)
		}
	}
	if _, exists := actualEnv[githubConfigEnv]; exists && !spec.WorkspaceGH {
		return fmt.Errorf("%w: GitHub CLI environment is present outside workspace gh mode", ErrSpecDrift)
	}

	expected := make(map[string]mount.Mount, 3)
	for _, expectedMount := range specMounts(spec) {
		expected[expectedMount.Target] = expectedMount
	}
	tallies := make(map[string]*observedMount, len(expected))
	for _, actualMount := range info.Mounts {
		tally := tallies[actualMount.Destination]
		if tally == nil {
			tally = &observedMount{}
			tallies[actualMount.Destination] = tally
		}
		tally.count++
		switch actualMount.Destination {
		case repoMountTarget:
			if actualMount.Type != mount.TypeBind || !actualMount.RW {
				return fmt.Errorf("%w: repository mount type or access was modified", ErrSpecDrift)
			}
			tally.source = actualMount.Source
		case dataMountTarget:
			if actualMount.Type != mount.TypeVolume || !actualMount.RW {
				return fmt.Errorf("%w: data mount type or access was modified", ErrSpecDrift)
			}
			tally.name = actualMount.Name
		case githubConfigDir:
			if actualMount.Type != mount.TypeVolume || !actualMount.RW {
				return fmt.Errorf("%w: GitHub CLI config mount type or access was modified", ErrSpecDrift)
			}
			tally.name = actualMount.Name
		}
	}
	repo, data, gh := tallies[repoMountTarget], tallies[dataMountTarget], tallies[githubConfigDir]
	ghDrift := gh != nil
	if spec.WorkspaceGH {
		ghDrift = gh == nil || gh.count != 1 || gh.name != specGHVolumeName(spec)
	}
	if len(info.Mounts) != len(expected) || repo == nil || repo.count != 1 || data == nil || data.count != 1 ||
		filepath.Clean(repo.source) != filepath.Clean(spec.RepoPath) || data.name != specDataVolumeName(spec) || ghDrift {
		return fmt.Errorf("%w: repository, data, or GitHub CLI config mount was modified", ErrSpecDrift)
	}
	return nil
}
