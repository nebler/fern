package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/nebler/fern/internal/hostlease"
)

func TestBackupLoadsOnlyCurrentConfiguration(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "fern.yaml")
	envPath := filepath.Join(directory, "fern.env")
	data, err := os.ReadFile("../../fern.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	current := strings.Replace(string(data), "/srv/fern/repository", directory, 1)
	current = strings.Replace(current, "sha256:REPLACE_WITH_QUALIFIED_LOCAL_IMAGE_ID", "sha256:"+strings.Repeat("b", 64), 1)
	if err := os.WriteFile(envPath, []byte("FERN_CONTROL_PASSWORD="+strings.Repeat("s", 32)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(current), 0600); err != nil {
		t.Fatal(err)
	}
	options := backupOptions{configPath: path, envPath: envPath}
	cfg, name, err := loadBackupConfig(options)
	if err != nil {
		t.Fatal(err)
	}
	if name != "default" || cfg.Workspace.Repo != directory || cfg.Workspace.GitHub.Repository.ID != 123456789 || cfg.ControlPassword != strings.Repeat("s", 32) {
		t.Fatalf("backup configuration = %+v, %q", cfg, name)
	}
	for _, retired := range []string{"  image: retired\n", "  memory: 8Gi\n", "  env: {}\n"} {
		if err := os.WriteFile(path, []byte(strings.Replace(current, "workspace:\n", "workspace:\n"+retired, 1)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadBackupConfig(options); err == nil {
			t.Fatalf("backup accepted retired setting %q", retired)
		}
	}
}

type backupFixture struct {
	options   backupOptions
	identity  *age.X25519Identity
	recipient []age.Recipient
}

func newBackupFixture(t *testing.T) backupFixture {
	t.Helper()
	root := t.TempDir()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return backupFixture{
		options: backupOptions{
			stateDirectory: filepath.Join(root, "home", ".fern"),
			configPath:     filepath.Join(root, "etc", "fern.yaml"),
			envPath:        filepath.Join(root, "etc", "fern.env"),
		},
		identity:  identity,
		recipient: []age.Recipient{identity.Recipient()},
	}
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// seed writes a live Fern state tree with durable authority, secrets, and
// disposable scratch, and returns the task database path.
func (fixture backupFixture) seed(t *testing.T, marker string) string {
	t.Helper()
	state := fixture.options.stateDirectory
	for relative, value := range map[string]string{
		"github-app/app-credentials.json":                            `{"private_key":"secret-app-key"}`,
		"tasks/demo-background/artifact-cas/sha256:abc/manifest":     "artifact-" + marker,
		"tasks/demo-background/artifact-work/scratch":                "scratch",
		"tasks/demo-background/runtime/background-runs/host.key":     "secret-host-key-0123456789abcdef",
		"tasks/demo-background/runtime/background-runs/clone/README": "clone",
		"locks/other.lock": "",
	} {
		writeTestFile(t, filepath.Join(state, relative), value)
	}
	writeTestFile(t, fixture.options.configPath, "workspace:\n  name: demo\n")
	writeTestFile(t, fixture.options.envPath, "FERN_CONTROL_PASSWORD=secret-control-password\n")
	database := filepath.Join(state, "tasks", "demo.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(database)+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"CREATE TABLE IF NOT EXISTS runs (id TEXT)",
		"CREATE TABLE IF NOT EXISTS devices (name TEXT)",
		"DELETE FROM runs",
		"DELETE FROM devices",
		"INSERT INTO runs VALUES ('" + marker + "')",
		"INSERT INTO devices VALUES ('devices-" + marker + "')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func readRun(t *testing.T, database string) string {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(database))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id string
	if err := db.QueryRow("SELECT id FROM runs").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBackupRoundTripEncryptsSecretsAndRefusesExistingState(t *testing.T) {
	ctx := context.Background()
	source := newBackupFixture(t)
	source.seed(t, "a")
	output := filepath.Join(t.TempDir(), "fern.backup")
	manifest, err := createBackup(ctx, source.options, "demo", output, source.recipient)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-app-key", "secret-host-key", "secret-control-password", "devices-a"} {
		if bytes.Contains(ciphertext, []byte(secret)) {
			t.Fatalf("backup contains plaintext %q", secret)
		}
	}
	if info, err := os.Stat(output); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, %v", info, err)
	}
	if _, err := createBackup(ctx, source.options, "demo", output, source.recipient); err == nil {
		t.Fatal("backup replaced an existing output")
	}
	paths := make(map[string]bool)
	for _, entry := range manifest.Files {
		paths[entry.Path] = true
	}
	for _, excluded := range []string{"state/locks/other.lock", "state/tasks/demo.db-wal",
		"state/tasks/demo-background/artifact-work/scratch", "state/tasks/demo-background/runtime/background-runs/clone/README"} {
		if paths[excluded] {
			t.Fatalf("backup captured disposable %s", excluded)
		}
	}

	// Restore onto a fresh host.
	target := newBackupFixture(t)
	target.options.stateDirectory = filepath.Join(t.TempDir(), ".fern")
	if _, err := restoreBackup(ctx, target.options, output, []age.Identity{source.identity}); err != nil {
		t.Fatal(err)
	}
	state := target.options.stateDirectory
	for relative, want := range map[string]string{
		"github-app/app-credentials.json":                        `{"private_key":"secret-app-key"}`,
		"tasks/demo-background/artifact-cas/sha256:abc/manifest": "artifact-a",
		"tasks/demo-background/runtime/background-runs/host.key": "secret-host-key-0123456789abcdef",
	} {
		if got := readTestFile(t, filepath.Join(state, relative)); got != want {
			t.Fatalf("%s = %q, want %q", relative, got, want)
		}
	}
	if got := readRun(t, filepath.Join(state, "tasks", "demo.db")); got != "a" {
		t.Fatalf("restored run = %q", got)
	}
	if !strings.Contains(readTestFile(t, target.options.envPath), "secret-control-password") ||
		readTestFile(t, target.options.configPath) != "workspace:\n  name: demo\n" {
		t.Fatal("configuration was not restored")
	}
	if pathExists(filepath.Join(state, "tasks/demo-background/artifact-work")) {
		t.Fatal("restore produced disposable state")
	}

	// Restore never replaces existing state or configuration.
	database := source.seed(t, "b")
	if _, err := restoreBackup(ctx, source.options, output, []age.Identity{source.identity}); err == nil ||
		!strings.Contains(err.Error(), "Fern state exists") {
		t.Fatalf("restore over live state = %v", err)
	}
	if readRun(t, database) != "b" {
		t.Fatal("refused restore changed live state")
	}
	if err := os.RemoveAll(source.options.stateDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreBackup(ctx, source.options, output, []age.Identity{source.identity}); err == nil ||
		!strings.Contains(err.Error(), source.options.configPath+" exists") {
		t.Fatalf("restore over existing configuration = %v", err)
	}
	if pathExists(source.options.stateDirectory) {
		t.Fatal("refused restore created state")
	}
}

func TestBackupRefusesWhileFernHoldsTheLease(t *testing.T) {
	ctx := context.Background()
	fixture := newBackupFixture(t)
	fixture.seed(t, "a")
	output := filepath.Join(t.TempDir(), "fern.backup")
	for _, name := range []string{"demo", "other-workspace"} {
		lease, err := hostlease.Acquire(filepath.Join(fixture.options.stateDirectory, "locks"), name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := createBackup(ctx, fixture.options, "demo", output, fixture.recipient); err == nil || !strings.Contains(err.Error(), "Fern must be stopped") {
			t.Fatalf("backup with %s lease held = %v", name, err)
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if pathExists(output) {
		t.Fatal("refused backup left output behind")
	}
}

type testEntry struct {
	name, body string
	typeflag   byte
}

func writeTestBackup(t *testing.T, recipients []age.Recipient, entries []testEntry, manifest *backupManifest) string {
	t.Helper()
	var buffer bytes.Buffer
	encrypted, err := age.Encrypt(&buffer, recipients...)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(encrypted)
	archive := tar.NewWriter(compressed)
	add := func(entry testEntry) {
		header := &tar.Header{Name: entry.name, Mode: 0o600, Typeflag: entry.typeflag, ModTime: time.Unix(0, 0)}
		if entry.typeflag == tar.TypeSymlink {
			header.Linkname = "/etc/passwd"
		} else {
			header.Size = int64(len(entry.body))
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range entries {
		add(entry)
	}
	if manifest != nil {
		encoded, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		add(testEntry{name: backupManifestName, body: string(encoded), typeflag: tar.TypeReg})
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "crafted.backup")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func checksum(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestBackupRestoreRejectsTamperedOrUnsafeArchives(t *testing.T) {
	ctx := context.Background()
	fixture := newBackupFixture(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	config := testEntry{name: backupConfigEntry, body: "workspace: {}\n", typeflag: tar.TypeReg}
	valid := func(extra ...testEntry) *backupManifest {
		manifest := &backupManifest{Format: backupFormat, Workspace: "demo"}
		for _, entry := range append([]testEntry{config}, extra...) {
			manifest.Files = append(manifest.Files, backupFileEntry{Path: entry.name, SHA256: checksum(entry.body)})
		}
		return manifest
	}
	stateFile := testEntry{name: "state/github-app/app-credentials.json", body: "credentials", typeflag: tar.TypeReg}
	tampered := valid(stateFile)
	tampered.Files[1].SHA256 = checksum("other")
	unlisted := valid()

	good := writeTestBackup(t, fixture.recipient, []testEntry{config, stateFile}, valid(stateFile))
	ciphertext, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.backup")
	if err := os.WriteFile(truncated, ciphertext[:len(ciphertext)-20], 0o600); err != nil {
		t.Fatal(err)
	}
	flipped := bytes.Clone(ciphertext)
	flipped[len(flipped)/2] ^= 1
	corrupted := filepath.Join(t.TempDir(), "corrupted.backup")
	if err := os.WriteFile(corrupted, flipped, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		input    string
		identity age.Identity
		want     string
	}{
		"wrong identity": {good, other, "decrypt backup"},
		"truncated":      {truncated, fixture.identity, "read backup"},
		"corrupted":      {corrupted, fixture.identity, "backup"},
		"checksum": {writeTestBackup(t, fixture.recipient, []testEntry{config, stateFile}, tampered),
			fixture.identity, "checksum mismatch"},
		"unlisted file": {writeTestBackup(t, fixture.recipient, []testEntry{config, stateFile}, unlisted),
			fixture.identity, "do not match the manifest"},
		"missing manifest": {writeTestBackup(t, fixture.recipient, []testEntry{config}, nil),
			fixture.identity, "manifest"},
		"symlink": {writeTestBackup(t, fixture.recipient, []testEntry{config, {name: "state/link", typeflag: tar.TypeSymlink}}, valid()),
			fixture.identity, "not a regular file"},
		"path escape": {writeTestBackup(t, fixture.recipient, []testEntry{config, {name: "state/../../escaped", body: "x", typeflag: tar.TypeReg}}, valid()),
			fixture.identity, "unsafe backup entry"},
		"absolute path": {writeTestBackup(t, fixture.recipient, []testEntry{config, {name: "/tmp/escaped", body: "x", typeflag: tar.TypeReg}}, valid()),
			fixture.identity, "unsafe backup entry"},
		"unknown top level": {writeTestBackup(t, fixture.recipient, []testEntry{config, {name: "repository/x", body: "x", typeflag: tar.TypeReg}}, valid()),
			fixture.identity, "unsafe backup entry"},
		"duplicate": {writeTestBackup(t, fixture.recipient, []testEntry{config, stateFile, stateFile}, valid(stateFile)),
			fixture.identity, "duplicate backup entry"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := restoreBackup(ctx, fixture.options, test.input, []age.Identity{test.identity})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restore error = %v, want %q", err, test.want)
			}
			if pathExists(fixture.options.stateDirectory) || pathExists(fixture.options.configPath) {
				t.Fatal("rejected restore activated files")
			}
			parent := filepath.Dir(fixture.options.stateDirectory)
			if entries, _ := os.ReadDir(parent); len(entries) != 0 {
				t.Fatalf("rejected restore left staging behind: %v", entries)
			}
		})
	}
	if _, err := restoreBackup(ctx, fixture.options, good, []age.Identity{fixture.identity}); err != nil {
		t.Fatalf("well-formed crafted backup rejected: %v", err)
	}
}

func TestBackupIncludedKeepsAuthorityAndDropsDisposableWork(t *testing.T) {
	t.Parallel()
	for relative, want := range map[string]bool{
		"github-app/app-credentials.json": true,
		"tasks/demo.db":                   true,
		"tasks/demo.db-wal":               false,
		"tasks/demo.db-shm":               false,
		"tasks/demo-background/artifact-cas/sha256:a/manifest":        true,
		"tasks/demo-background/runtime":                               true,
		"tasks/demo-background/runtime/background-runs":               true,
		"tasks/demo-background/runtime/background-runs/host.key":      true,
		"tasks/demo-background/runtime/background-runs/clone":         false,
		"tasks/demo-background/runtime/background-runs/.clone-lock-x": false,
		"tasks/demo-background/artifact-work":                         false,
		"locks":                                                       false,
		"locks/abc.lock":                                              false,
	} {
		if got := backupIncluded(relative); got != want {
			t.Errorf("backupIncluded(%q) = %v, want %v", relative, got, want)
		}
	}
}

// TestBackupCommandsRoundTripAHost drives `fern backup create` and `fern backup
// restore` through the CLI with a real configuration, as an operator would when
// moving a stopped host.
func TestBackupCommandsRoundTripAHost(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../fern.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	configuration := strings.Replace(string(example), "/srv/fern/repository", repository, 1)
	configuration = strings.Replace(configuration, "sha256:REPLACE_WITH_QUALIFIED_LOCAL_IMAGE_ID", "sha256:"+strings.Repeat("b", 64), 1)
	host := func(name string) backupOptions {
		return backupOptions{
			stateDirectory: filepath.Join(root, name, "home", ".fern"),
			configPath:     filepath.Join(root, name, "etc", "fern.yaml"),
			envPath:        filepath.Join(root, name, "etc", "fern.env"),
		}
	}
	// The state directory is always ~/.fern, so each host gets its own HOME.
	flags := func(options backupOptions) []string {
		t.Setenv("HOME", filepath.Dir(options.stateDirectory))
		return []string{"--config", options.configPath, "--env-file", options.envPath}
	}
	source := backupFixture{options: host("source")}
	source.seed(t, "a")
	writeTestFile(t, source.options.configPath, configuration)
	writeTestFile(t, source.options.envPath, "FERN_CONTROL_PASSWORD=secret-control-password-0123456789\n")

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(root, "identity.txt")
	writeTestFile(t, identityPath, identity.String()+"\n")
	output := filepath.Join(root, "fern.backup")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	create := append([]string{"backup", "create"}, flags(source.options)...)
	create = append(create, "--recipient", identity.Recipient().String(), "--output", output)
	if err := run(create, log); err != nil {
		t.Fatal(err)
	}
	if err := run(create, log); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second backup create = %v", err)
	}

	target := host("target")
	restore := append([]string{"backup", "restore"}, flags(target)...)
	restore = append(restore, "--identity", identityPath, "--input", output)
	if err := run(restore, log); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"github-app/app-credentials.json", "tasks/demo-background/runtime/background-runs/host.key"} {
		if readTestFile(t, filepath.Join(target.stateDirectory, path)) != readTestFile(t, filepath.Join(source.options.stateDirectory, path)) {
			t.Fatalf("restored %s differs", path)
		}
	}
	if readTestFile(t, target.configPath) != configuration || readTestFile(t, target.envPath) != readTestFile(t, source.options.envPath) {
		t.Fatal("restored configuration differs")
	}
	if got := readRun(t, filepath.Join(target.stateDirectory, "tasks", "demo.db")); got != "a" {
		t.Fatalf("restored run = %q", got)
	}
	if pathExists(filepath.Join(target.stateDirectory, "tasks/demo-background/artifact-work")) {
		t.Fatal("restore produced disposable scratch")
	}
	if err := run(restore, log); err == nil || !strings.Contains(err.Error(), "Fern state exists") {
		t.Fatalf("restore over a restored host = %v", err)
	}
}
