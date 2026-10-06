package taskenvdocker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeRootCopiesDurableAuthoritativeHostKey(t *testing.T) {
	p, docker, _ := testProvider(t)
	durablePath := filepath.Join(p.config.StateRoot, runRootName, hostKeyName)
	before, err := os.ReadFile(durablePath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := p.config
	config.RuntimeStorageRoot = runtimeRoot
	for i := 0; i < 2; i++ {
		candidate, err := New(context.Background(), config, docker)
		if err != nil {
			t.Fatal(err)
		}
		if candidate.hostKey != p.hostKey {
			t.Fatal("runtime provider changed durable host identity")
		}
		if candidate.root != filepath.Join(runtimeRoot, runRootName) {
			t.Fatal("runtime root differs")
		}
		if err := candidate.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{durablePath, filepath.Join(runtimeRoot, runRootName, hostKeyName)} {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, before) {
			t.Fatalf("host key copy differs at %s: %v", path, err)
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("host key permissions at %s: %v", path, err)
		}
		parent, err := os.Lstat(filepath.Dir(path))
		if err != nil || parent.Mode().Perm() != 0700 {
			t.Fatalf("host key parent permissions at %s: %v", path, err)
		}
	}
}

func TestRuntimeRootRejectsDifferentHostKeyWithoutChangingDurableKey(t *testing.T) {
	p, docker, _ := testProvider(t)
	runtimeRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Model an existing runtime root created with an unrelated host identity.
	root, wrongKey, err := prepareRoot(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if wrongKey == p.hostKey {
		t.Fatal("unexpected random key collision")
	}
	config := p.config
	config.RuntimeStorageRoot = runtimeRoot
	if candidate, err := New(context.Background(), config, docker); err == nil {
		candidate.Close()
		t.Fatal("unrelated runtime host key accepted")
	}
	for path, want := range map[string][32]byte{
		filepath.Join(p.config.StateRoot, runRootName): p.hostKey,
		root: wrongKey,
	} {
		got, err := loadExistingRoot(path)
		if err != nil || got != want {
			t.Fatalf("mismatch handling changed existing identity at %s: %v", path, err)
		}
	}
}

func TestQuotaKernelStateRequiresEnforcedFiniteInheritedProject(t *testing.T) {
	type state struct {
		project, xflags  uint32
		version          byte
		flags            uint16
		qversion, qflags byte
		qproject         uint32
		blocks, inodes   uint64
	}
	valid := state{23, 0x200, 1, 0x30, 1, 2, 23, 8192, 1000}
	check := func(s state) error {
		return validateQuotaKernelState(s.project, s.xflags, s.version, s.flags, s.qversion, s.qflags, s.qproject, s.blocks, s.inodes)
	}
	if err := check(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*state){
		"project zero":           func(s *state) { s.project = 0 },
		"no inheritance":         func(s *state) { s.xflags = 0 },
		"accounting only":        func(s *state) { s.flags = 0x10 },
		"enforcement only":       func(s *state) { s.flags = 0x20 },
		"no quota":               func(s *state) { s.flags = 0 },
		"unknown status version": func(s *state) { s.version = 2 },
		"unknown quota version":  func(s *state) { s.qversion = 2 },
		"user quota":             func(s *state) { s.qflags = 1 },
		"different project":      func(s *state) { s.qproject++ },
		"unlimited bytes":        func(s *state) { s.blocks = 0 },
		"unlimited inodes":       func(s *state) { s.inodes = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			mutate(&s)
			if check(s) == nil {
				t.Fatal("unsafe quota admitted")
			}
		})
	}
}

func TestQuotaAdmissionFailsBeforeExecutionMutation(t *testing.T) {
	p, docker, run := testProvider(t)
	before, err := os.ReadDir(p.root)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("quota enforcement unavailable")
	p.config.quotaCheck = func(string) (quotaIdentity, error) { return quotaIdentity{}, denied }
	for name, operation := range map[string]func() error{
		"clone":     func() error { _, err := p.EnsureClone(context.Background(), run); return err },
		"volume":    func() error { _, err := p.EnsureVolume(context.Background(), run); return err },
		"container": func() error { _, err := p.EnsureContainer(context.Background(), run); return err },
		"start":     func() error { _, err := p.StartContainer(context.Background(), run, "test"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(operation(), denied) {
				t.Fatal("execution did not fail at quota admission")
			}
		})
	}
	after, err := os.ReadDir(p.root)
	if err != nil || len(after) != len(before) {
		t.Fatalf("admission mutated root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.root, run.CloneIdentity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone mutation: %v", err)
	}
	// Provider reconstruction and cleanup identity remain available even when
	// enforcement is gone; a full disk must not disable durable recovery.
	reopened, err := New(context.Background(), p.config, docker)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.cleanupDigest(run); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaAdmissionRejectsDifferentCloneProject(t *testing.T) {
	p, _, _ := testProvider(t)
	p.config.quotaCheck = func(path string) (quotaIdentity, error) {
		q := quotaIdentity{Device: 1, Project: 1, Blocks: 1024, Inodes: 100}
		if path == p.root {
			q.Project++
		}
		return q, nil
	}
	if p.admitStorage() == nil {
		t.Fatal("different clone project admitted")
	}
}

func TestQuotaPlatformAndMissingPathFailClosed(t *testing.T) {
	_, err := inspectProjectQuota(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("missing quota directory admitted")
	}
	if runtime.GOOS != "linux" && !strings.Contains(err.Error(), "Linux") {
		t.Fatalf("platform error: %v", err)
	}
}

func TestQuotaVolumeRejectsLegacyAdoptionButAllowsCleanupIdentity(t *testing.T) {
	p, docker, run := testProvider(t)
	if _, err := p.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EnsureVolume(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	item, err := docker.VolumeInspect(context.Background(), run.VolumeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := p.cleanupDigest(run)
	if err != nil {
		t.Fatal(err)
	}
	p.config.RuntimeStorageRoot = p.config.StateRoot
	if err := p.attestVolume(run, digest, item); err != nil {
		t.Fatalf("legacy cleanup blocked: %v", err)
	}
	if err := p.attestExecutionVolume(run, item); err == nil {
		t.Fatal("legacy managed volume adopted for execution")
	}
	want := filepath.Join(p.root, "opencode-volumes", run.VolumeIdentity)
	if p.volumeOptions(run)["device"] != want {
		t.Fatal("volume backing path differs")
	}
}

func TestWorkerSeccompCannotChangeProjectQuota(t *testing.T) {
	var profile struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names  []string `json:"names"`
			Action string   `json:"action"`
			Args   []struct {
				Index int    `json:"index"`
				Value uint64 `json:"value"`
				Op    string `json:"op"`
			} `json:"args"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(workerSecurityOptions()[1], "seccomp=")), &profile); err != nil {
		t.Fatal(err)
	}
	if profile.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatal("unknown syscalls must fail closed")
	}
	for _, rule := range profile.Syscalls {
		for _, name := range rule.Names {
			if rule.Action != "SCMP_ACT_ALLOW" {
				continue
			}
			if name == "io_uring_setup" || name == "file_setattr" {
				t.Fatalf("unmediated file attribute API allowed: %s", name)
			}
			if name != "ioctl" {
				continue
			}
			if len(rule.Args) != 1 || rule.Args[0].Index != 1 || rule.Args[0].Op != "SCMP_CMP_EQ" {
				t.Fatal("unrestricted ioctl allowed")
			}
			for _, forbidden := range []uint64{0x401c5820, 0x40086602, 0x40046602} {
				if rule.Args[0].Value == forbidden {
					t.Fatal("quota-changing ioctl allowed")
				}
			}
		}
	}
}

func TestReadonlyStoragePolicyAndLegacyCleanup(t *testing.T) {
	p, docker, run := preparedProvider(t)
	if _, err := p.EnsureContainer(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	info, err := docker.ContainerInspect(context.Background(), run.ContainerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := p.cleanupDigest(run)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HostConfig.ReadonlyRootfs || !equalMap(info.HostConfig.Tmpfs, workerTmpfs()) {
		t.Fatal("worker has unbounded writable root storage")
	}
	// Storage policy is Fern's own create request; attestation checks identity
	// and never blocks cleanup of a container created under another policy.
	info.HostConfig.ReadonlyRootfs = false
	info.HostConfig.Tmpfs = nil
	info.HostConfig.SecurityOpt = []string{"no-new-privileges"}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		t.Fatalf("legacy cleanup blocked: %v", err)
	}
}
