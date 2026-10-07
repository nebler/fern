package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/hostlease"
	_ "modernc.org/sqlite"
)

const (
	// A backup is one age-encrypted gzip tar holding state/..., the
	// configuration and protected environment under config/, and MANIFEST.json
	// with a sha256 for every file. Nothing in it is ever written in plaintext.
	backupFormat       = "fern-backup-v2"
	backupManifestName = "MANIFEST.json"
	backupConfigEntry  = "config/fern.yaml"
	backupEnvEntry     = "config/fern.env"
	maxBackupManifest  = 64 << 20
)

type backupOptions struct {
	configPath, envPath, stateDirectory string
}

type backupManifest struct {
	Format    string            `json:"format"`
	CreatedAt time.Time         `json:"created_at"`
	Workspace string            `json:"workspace"`
	Files     []backupFileEntry `json:"files"`
}

type backupFileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// backupFlags registers the shared flags; the returned function resolves them
// after parsing. The state directory is always ~/.fern of the invoking user,
// exactly as for fern up.
func backupFlags(command, description string) (*flag.FlagSet, func() (backupOptions, error)) {
	fs := newFlagSet(command, description)
	configPath, envPath := addConfigFlags(fs)
	return fs, func() (backupOptions, error) {
		state, err := statePath("")
		if err != nil {
			return backupOptions{}, fmt.Errorf("determine Fern state directory: %w", err)
		}
		return backupOptions{configPath: *configPath, envPath: *envPath, stateDirectory: filepath.Clean(state)}, nil
	}
}

func runBackupCreate(args []string, _ *slog.Logger) error {
	fs, resolveOptions := backupFlags("backup create", "Create an age-encrypted offline Fern host backup.")
	output := fs.String("output", "", "encrypted backup file to create (required)")
	var recipientFlags repeatedFlag
	fs.Var(&recipientFlags, "recipient", "age X25519 recipient (repeatable, required)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *output == "" || len(recipientFlags) == 0 {
		return invocationError{message: "--output and at least one --recipient are required"}
	}
	options, err := resolveOptions()
	if err != nil {
		return err
	}
	recipients, err := parseAgeRecipients(recipientFlags)
	if err != nil {
		return err
	}
	cfg, err := loadBackupConfig(options)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	manifest, err := createBackup(ctx, options, cfg.Workspace.Name, *output, recipients)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "created encrypted backup %s (%d files)\n", *output, len(manifest.Files))
	return err
}

func runBackupRestore(args []string, _ *slog.Logger) error {
	fs, resolveOptions := backupFlags("backup restore", "Verify an encrypted backup and install it on a host with no Fern state or configuration.")
	input := fs.String("input", "", "encrypted backup file (required)")
	var identityPaths repeatedFlag
	fs.Var(&identityPaths, "identity", "private age X25519 identity file (repeatable, required)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *input == "" || len(identityPaths) == 0 {
		return invocationError{message: "--input and at least one --identity are required"}
	}
	options, err := resolveOptions()
	if err != nil {
		return err
	}
	identities, err := loadAgeIdentities(identityPaths)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	manifest, err := restoreBackup(ctx, options, *input, identities)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "restored backup of %q created %s; Fern remains stopped\n", manifest.Workspace, manifest.CreatedAt.Format(time.RFC3339))
	return err
}

// loadBackupConfig needs only the workspace binding, not live credentials.
func loadBackupConfig(options backupOptions) (config.Config, error) {
	cfg, err := loadCommandConfig(options.configPath, options.envPath)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, config.ValidateWorkspace(cfg)
}

// backupLeases holds the named workspace lease plus every other lease file in
// the state directory, so no `fern up` sharing this state can run meanwhile.
type backupLeases struct {
	lease *hostlease.Lease
	files []*os.File
}

func acquireBackupLeases(stateDirectory, workspace string) (*backupLeases, error) {
	directory := filepath.Join(stateDirectory, "locks")
	held := &backupLeases{}
	own := ""
	if workspace != "" {
		lease, err := hostlease.Acquire(directory, workspace)
		if err != nil {
			return nil, fmt.Errorf("Fern must be stopped: %w", err)
		}
		held.lease = lease
		own = fmt.Sprintf("%x.lock", sha256.Sum256([]byte(workspace)))
	}
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		held.release()
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() == own || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		file, err := os.OpenFile(filepath.Join(directory, entry.Name()), os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err == nil {
			if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				_ = file.Close()
			}
		}
		if err != nil {
			held.release()
			return nil, fmt.Errorf("Fern must be stopped: lease %s is held: %w", entry.Name(), err)
		}
		held.files = append(held.files, file)
	}
	return held, nil
}

func (held *backupLeases) release() {
	for _, file := range held.files {
		_ = file.Close()
	}
	_ = held.lease.Release()
}

// backupIncluded reports whether a slash-separated state-relative path is
// durable state. Locks, SQLite sidecars (databases are snapshotted), artifact
// scratch, and runtime clones are disposable; of the runtime root only the
// host key is kept.
func backupIncluded(relative string) bool {
	parts := strings.Split(relative, "/")
	if parts[0] == "locks" || isSQLiteSidecar(parts[len(parts)-1]) {
		return false
	}
	if len(parts) >= 3 && parts[0] == "runs" && strings.HasSuffix(parts[1], "-background") {
		switch parts[2] {
		case "artifact-work":
			return false
		case "runtime":
			rest := strings.Join(parts[3:], "/")
			return rest == "" || rest == "background-runs" || rest == "background-runs/host.key"
		}
	}
	return true
}

func isSQLite(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".db", ".sqlite", ".sqlite3":
		return true
	}
	return false
}

func isSQLiteSidecar(name string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && isSQLite(base) {
			return true
		}
	}
	return false
}

func createBackup(ctx context.Context, options backupOptions, workspace, output string, recipients []age.Recipient) (backupManifest, error) {
	manifest := backupManifest{Format: backupFormat, CreatedAt: time.Now().UTC().Truncate(time.Second), Workspace: workspace}
	if pathExists(output) {
		return manifest, fmt.Errorf("backup output already exists: %s", output)
	}
	if pathContains(options.stateDirectory, output) {
		return manifest, errors.New("backup output must be outside the Fern state directory")
	}
	leases, err := acquireBackupLeases(options.stateDirectory, workspace)
	if err != nil {
		return manifest, err
	}
	defer leases.release()

	scratch, err := os.MkdirTemp(filepath.Dir(options.stateDirectory), ".fern-backup-")
	if err != nil {
		return manifest, err
	}
	defer os.RemoveAll(scratch)
	temporary, err := os.CreateTemp(filepath.Dir(output), ".fern-backup-*.tmp")
	if err != nil {
		return manifest, err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if err := temporary.Chmod(0o600); err != nil {
		return manifest, err
	}
	encrypted, err := age.Encrypt(temporary, recipients...)
	if err != nil {
		return manifest, err
	}
	compressed := gzip.NewWriter(encrypted)
	archive := tar.NewWriter(compressed)

	add := func(name, source string) error {
		file, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("backup source is not a regular file: %s", source)
		}
		header := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime().UTC()}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		hash := sha256.New()
		if _, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(file, info.Size())); err != nil {
			return fmt.Errorf("archive %s: %w", source, err)
		}
		manifest.Files = append(manifest.Files, backupFileEntry{Path: name, SHA256: hex.EncodeToString(hash.Sum(nil))})
		return nil
	}

	snapshots := 0
	err = filepath.WalkDir(options.stateDirectory, func(source string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || source == options.stateDirectory {
			return walkErr
		}
		relative, err := filepath.Rel(options.stateDirectory, source)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !backupIncluded(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case entry.IsDir():
			return nil
		case !entry.Type().IsRegular():
			return fmt.Errorf("link or special file rejected in Fern state: %s", source)
		case isSQLite(relative):
			snapshots++
			snapshot := filepath.Join(scratch, fmt.Sprintf("%d.db", snapshots))
			if err := snapshotSQLite(ctx, source, snapshot); err != nil {
				return err
			}
			return add("state/"+relative, snapshot)
		default:
			return add("state/"+relative, source)
		}
	})
	if err != nil {
		return manifest, err
	}
	if err := add(backupConfigEntry, options.configPath); err != nil {
		return manifest, err
	}
	if pathExists(options.envPath) {
		if err := add(backupEnvEntry, options.envPath); err != nil {
			return manifest, err
		}
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	header := &tar.Header{Typeflag: tar.TypeReg, Name: backupManifestName, Mode: 0o600, Size: int64(len(encoded)), ModTime: manifest.CreatedAt}
	if err := archive.WriteHeader(header); err != nil {
		return manifest, err
	}
	if _, err := archive.Write(encoded); err != nil {
		return manifest, err
	}
	if err := errors.Join(archive.Close(), compressed.Close(), encrypted.Close(), temporary.Sync(), temporary.Close()); err != nil {
		return manifest, err
	}
	// Link refuses to replace an output created concurrently.
	if err := os.Link(temporary.Name(), output); err != nil {
		return manifest, fmt.Errorf("install backup: %w", err)
	}
	return manifest, syncDirectory(filepath.Dir(output))
}

func sqliteDSN(path string) string {
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := dsn.Query()
	query.Set("mode", "rw")
	query.Add("_pragma", "busy_timeout(5000)")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

// snapshotSQLite writes a consistent, WAL-free copy with VACUUM INTO and
// integrity-checks the copy.
func snapshotSQLite(ctx context.Context, source, target string) error {
	database, err := sql.Open("sqlite", sqliteDSN(source))
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, "VACUUM INTO ?", target)
	if err = errors.Join(err, database.Close()); err != nil {
		return fmt.Errorf("snapshot SQLite state %s: %w", source, err)
	}
	return checkSQLite(ctx, target)
}

func checkSQLite(ctx context.Context, path string) error {
	database, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return err
	}
	var integrity string
	err = database.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity)
	if err == nil && integrity != "ok" {
		err = fmt.Errorf("integrity_check returned %q", integrity)
	}
	if err = errors.Join(err, database.Close()); err != nil {
		return fmt.Errorf("check SQLite state %s: %w", path, err)
	}
	return nil
}

// restoreBackup installs a verified backup on a host with no Fern state or
// configuration. It never replaces existing files: an operator restoring over a
// host must move or delete the old state and configuration first.
func restoreBackup(ctx context.Context, options backupOptions, input string, identities []age.Identity) (backupManifest, error) {
	state := filepath.Clean(options.stateDirectory)
	live, err := hasLiveState(state)
	if err != nil {
		return backupManifest{}, err
	}
	if live {
		return backupManifest{}, fmt.Errorf("Fern state exists at %s; move it aside or delete it before restoring", state)
	}
	for _, target := range []string{options.configPath, options.envPath} {
		if pathExists(target) {
			return backupManifest{}, fmt.Errorf("%s exists; move it aside or delete it before restoring", target)
		}
	}
	file, err := os.Open(input)
	if err != nil {
		return backupManifest{}, err
	}
	defer file.Close()
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		return backupManifest{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(state), ".fern-restore-")
	if err != nil {
		return backupManifest{}, err
	}
	defer os.RemoveAll(staging)
	manifest, err := extractBackup(file, identities, staging)
	if err != nil {
		return manifest, err
	}
	stagedState := filepath.Join(staging, "state")
	if err := os.MkdirAll(stagedState, 0o700); err != nil {
		return manifest, err
	}
	err = filepath.WalkDir(stagedState, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !isSQLite(path) {
			return walkErr
		}
		return checkSQLite(ctx, path)
	})
	if err != nil {
		return manifest, err
	}
	leases, err := acquireBackupLeases(state, manifest.Workspace)
	if err != nil {
		return manifest, err
	}
	defer leases.release()

	// The held lease files move into the new state so the lease stays in force;
	// the old directory holds nothing else and is discarded.
	steps := []renameStep{
		{filepath.Join(state, "locks"), filepath.Join(stagedState, "locks")},
		{state, state + ".discard"},
		{stagedState, state},
	}
	files := map[string]string{options.configPath: backupConfigEntry, options.envPath: backupEnvEntry}
	for target, entry := range files {
		source := filepath.Join(staging, filepath.FromSlash(entry))
		if !pathExists(source) {
			continue
		}
		// Copy next to the target so the final rename stays on one filesystem.
		incoming := target + ".restore"
		if err := os.RemoveAll(incoming); err != nil {
			return manifest, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return manifest, err
		}
		if err := copyPath(source, incoming); err != nil {
			return manifest, err
		}
		defer os.Remove(incoming)
		steps = append(steps, renameStep{incoming, target})
	}
	return manifest, applyRenames(steps)
}

func hasLiveState(state string) (bool, error) {
	entries, err := os.ReadDir(state)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != "locks" {
			return true, nil
		}
	}
	return false, nil
}

// extractBackup authenticates, decompresses, and unpacks a backup into
// staging, accepting only regular files under state/ or config/ whose
// checksums exactly match the manifest.
func extractBackup(source io.Reader, identities []age.Identity, staging string) (backupManifest, error) {
	var manifest backupManifest
	decrypted, err := age.Decrypt(source, identities...)
	if err != nil {
		return manifest, fmt.Errorf("decrypt backup: %w", err)
	}
	compressed, err := gzip.NewReader(decrypted)
	if err != nil {
		return manifest, fmt.Errorf("read backup: %w", err)
	}
	archive := tar.NewReader(compressed)
	written := make(map[string]string)
	haveManifest := false
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return manifest, fmt.Errorf("read backup: %w", err)
		}
		name := header.Name
		if !validBackupEntry(name) {
			return manifest, fmt.Errorf("unsafe backup entry %q", name)
		}
		if header.Typeflag != tar.TypeReg {
			return manifest, fmt.Errorf("backup entry %q is not a regular file", name)
		}
		if _, duplicate := written[name]; duplicate || (haveManifest && name == backupManifestName) {
			return manifest, fmt.Errorf("duplicate backup entry %q", name)
		}
		if name == backupManifestName {
			decoder := json.NewDecoder(io.LimitReader(archive, maxBackupManifest))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&manifest); err != nil {
				return manifest, fmt.Errorf("invalid backup manifest: %w", err)
			}
			haveManifest = true
			continue
		}
		target := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return manifest, err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, header.FileInfo().Mode().Perm())
		if err != nil {
			return manifest, err
		}
		hash := sha256.New()
		_, err = io.Copy(io.MultiWriter(output, hash), archive)
		if err = errors.Join(err, output.Sync(), output.Close()); err != nil {
			return manifest, fmt.Errorf("extract %s: %w", name, err)
		}
		written[name] = hex.EncodeToString(hash.Sum(nil))
	}
	// Reading to the end makes gzip verify its checksum and age authenticate
	// the final chunk, so truncation is detected before anything is activated.
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return manifest, fmt.Errorf("read backup: %w", err)
	}
	if _, err := io.Copy(io.Discard, decrypted); err != nil {
		return manifest, fmt.Errorf("read backup: %w", err)
	}
	if !haveManifest || manifest.Format != backupFormat || manifest.Workspace == "" {
		return manifest, fmt.Errorf("backup has no %s manifest", backupFormat)
	}
	if len(manifest.Files) != len(written) {
		return manifest, errors.New("backup files do not match the manifest")
	}
	for _, entry := range manifest.Files {
		if actual, ok := written[entry.Path]; !ok || actual != entry.SHA256 {
			return manifest, fmt.Errorf("backup checksum mismatch: %s", entry.Path)
		}
	}
	if _, ok := written[backupConfigEntry]; !ok {
		return manifest, errors.New("backup has no configuration file")
	}
	return manifest, nil
}

func validBackupEntry(name string) bool {
	if name == backupManifestName || name == backupConfigEntry || name == backupEnvEntry {
		return true
	}
	relative, ok := strings.CutPrefix(name, "state/")
	return ok && relative != "" && path.Clean(name) == name && !strings.Contains(name, "\\") &&
		!strings.HasPrefix(relative, "../") && relative != ".."
}

type renameStep struct{ from, to string }

// applyRenames performs the renames in order, undoing completed ones if any
// fails, then removes discarded copies. Steps whose source is absent are skipped.
func applyRenames(steps []renameStep) error {
	for _, step := range steps {
		if strings.HasSuffix(step.to, ".discard") {
			if err := os.RemoveAll(step.to); err != nil {
				return err
			}
		}
	}
	var done []renameStep
	for _, step := range steps {
		if !pathExists(step.from) {
			continue
		}
		if err := os.Rename(step.from, step.to); err != nil {
			for index := len(done) - 1; index >= 0; index-- {
				err = errors.Join(err, os.Rename(done[index].to, done[index].from))
			}
			return fmt.Errorf("activate restored files: %w", err)
		}
		done = append(done, step)
	}
	var result error
	for _, step := range done {
		if strings.HasSuffix(step.to, ".discard") {
			result = errors.Join(result, os.RemoveAll(step.to))
		}
		if directory := filepath.Dir(step.to); pathExists(directory) {
			result = errors.Join(result, syncDirectory(directory))
		}
	}
	return result
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}

func copyPath(source, target string) error {
	input, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", source)
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Sync(), output.Close())
}

func pathContains(parent, child string) bool {
	parent, _ = filepath.Abs(parent)
	child, _ = filepath.Abs(child)
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
