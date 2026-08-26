package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/registry"
	fernruntime "github.com/nebler/fern/internal/runtime"
	backupscript "github.com/nebler/fern/scripts"
	_ "modernc.org/sqlite"
)

const (
	defaultBackupConfig = "/etc/fern/fern.yaml"
	defaultBackupEnv    = "/etc/fern/fern.env"
)

type backupDocker interface {
	Destroy(context.Context, string) error
	ExportManagedVolumes(context.Context, fernruntime.Spec, string) ([]string, error)
	RestoreManagedVolumes(context.Context, fernruntime.Spec, map[string]string, string) error
	Close() error
}

const operationalRollbackDirectory = "operational-rollback"

var openBackupDocker = func(log *slog.Logger) (backupDocker, error) { return newDocker(log) }

type backupOptions struct {
	configPath, envPath, stateDirectory string
	archiveTool                         string
}

func backupFlags(command, description string) (*flag.FlagSet, *backupOptions) {
	fs := newFlagSet(command, description)
	options := &backupOptions{}
	fs.StringVar(&options.configPath, "config", defaultBackupConfig, "configuration file")
	fs.StringVar(&options.envPath, "env-file", defaultBackupEnv, "protected environment file")
	defaultState, err := statePath("")
	if err == nil {
		options.stateDirectory = filepath.Clean(defaultState)
	}
	fs.StringVar(&options.stateDirectory, "state-dir", options.stateDirectory, "Fern state directory")
	fs.StringVar(&options.archiveTool, "archive-tool", "", "alternate fern-host-backup.py path")
	return fs, options
}

func runBackupCreate(args []string, log *slog.Logger) error {
	fs, options := backupFlags("backup create", "Create and verify an offline Fern host backup.")
	output := fs.String("output", "", "backup bundle output directory (required)")
	credentials := fs.String("credential-output", "", "separate credential and volume archive")
	generation := fs.String("generation", "", "backup generation identifier")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *output == "" {
		return invocationError{message: "-output is required"}
	}
	if options.stateDirectory == "" {
		return errors.New("cannot determine Fern state directory")
	}
	if *generation == "" {
		var err error
		*generation, err = newBackupGeneration()
		if err != nil {
			return err
		}
	}
	if *credentials == "" {
		*credentials = *output + ".credentials.tar"
	}
	cfg, spec, err := loadBackupSpec(*options)
	if err != nil {
		return err
	}
	lease, err := registry.Acquire(filepath.Join(options.stateDirectory, "locks"), spec.Name)
	if err != nil {
		return fmt.Errorf("backup requires the offline workspace lease: %w", err)
	}
	defer lease.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	docker, err := openBackupDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	if err := docker.Destroy(ctx, spec.Name); err != nil {
		return fmt.Errorf("quiesce workspace compute: %w", err)
	}
	if err := checkpointSQLiteTree(ctx, options.stateDirectory); err != nil {
		return err
	}

	staging, err := os.MkdirTemp("", ".fern-backup-create-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	stateSource := filepath.Join(staging, "state")
	configSource := filepath.Join(staging, "config")
	volumeSource := filepath.Join(staging, "volumes")
	for _, path := range []string{stateSource, configSource, volumeSource} {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
	}
	if err := stageFernState(options.stateDirectory, stateSource); err != nil {
		return err
	}
	if err := stageConfig(options.configPath, options.envPath, configSource); err != nil {
		return err
	}
	volumeNames, err := docker.ExportManagedVolumes(ctx, spec, volumeSource)
	if err != nil {
		return fmt.Errorf("export managed volumes: %w", err)
	}
	epoch, lockDirectory, err := ensureBackupEpoch(options.stateDirectory)
	if err != nil {
		return err
	}
	toolArgs := []string{"backup", "--lock-dir", lockDirectory, "--epoch", epoch,
		"--generation", *generation, "--output", *output, "--state", stateSource,
		"--config", configSource, "--repository", cfg.Workspace.Repo,
		"--credential-policy", "external", "--credential-output", *credentials}
	for _, name := range volumeNames {
		toolArgs = append(toolArgs, "--volume", name+"="+filepath.Join(volumeSource, name))
	}
	if err := runBackupArchiveTool(ctx, options.archiveTool, toolArgs, os.Stdout, os.Stderr); err != nil {
		return err
	}
	return nil
}

func runBackupRestore(args []string, log *slog.Logger) error {
	fs, options := backupFlags("backup restore", "Verify and transactionally restore an offline Fern host backup.")
	bundle := fs.String("backup", "", "backup bundle directory (required)")
	credentials := fs.String("credential-input", "", "separate credential and volume archive")
	recoveryDirectory := fs.String("recovery-dir", "", "staged generation and transaction receipt directory")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *bundle == "" {
		return invocationError{message: "-backup is required"}
	}
	if options.stateDirectory == "" {
		return errors.New("cannot determine Fern state directory")
	}
	credentialInput, err := credentialInputForRestore(*bundle, *credentials)
	if err != nil {
		return err
	}
	*credentials = credentialInput
	if *recoveryDirectory == "" {
		*recoveryDirectory = filepath.Join(options.stateDirectory, "recovery")
	}
	cfg, spec, err := loadBackupSpec(*options)
	if err != nil {
		return err
	}
	lease, err := registry.Acquire(filepath.Join(options.stateDirectory, "locks"), spec.Name)
	if err != nil {
		return fmt.Errorf("restore requires the offline workspace lease: %w", err)
	}
	defer lease.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	docker, err := openBackupDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	if err := docker.Destroy(ctx, spec.Name); err != nil {
		return fmt.Errorf("quiesce workspace compute: %w", err)
	}
	epoch, lockDirectory, err := ensureBackupEpoch(options.stateDirectory)
	if err != nil {
		return err
	}
	toolArgs := []string{"restore", "--lock-dir", lockDirectory, "--epoch", epoch,
		"--backup", *bundle, "--target", *recoveryDirectory}
	if *credentials != "" {
		toolArgs = append(toolArgs, "--credential-input", *credentials)
	}
	if err := runBackupArchiveTool(ctx, options.archiveTool, toolArgs, os.Stdout, os.Stderr); err != nil {
		return err
	}
	current := filepath.Join(*recoveryDirectory, "current")
	manifest, err := readStagedBackupManifest(filepath.Join(current, "BACKUP-MANIFEST.json"))
	if err != nil {
		return err
	}
	wantVolumes := fernruntime.ManagedVolumeNames(spec)
	if !slices.Equal(manifest.NamedVolumes, wantVolumes) {
		return fmt.Errorf("backup volumes %q do not match configured volumes %q", manifest.NamedVolumes, wantVolumes)
	}
	transaction, err := prepareFilesystemRestore(current, options.stateDirectory, options.configPath, options.envPath, cfg.Workspace.Repo, manifest.Generation)
	if err != nil {
		return err
	}
	rollbackGeneration, err := newBackupGeneration()
	if err != nil {
		transaction.Cleanup()
		return err
	}
	rollbackRoot, err := createOperationalRollback(ctx, docker, spec, *recoveryDirectory, rollbackGeneration, manifest.Generation, transaction.paths)
	if err != nil {
		transaction.Cleanup()
		return err
	}
	if err := transaction.Activate(); err != nil {
		transaction.Cleanup()
		return err
	}
	if err := checkpointSQLiteTree(ctx, options.stateDirectory); err != nil {
		return errors.Join(fmt.Errorf("validate restored filesystem generation: %w", err), transaction.Rollback())
	}
	volumeSources := make(map[string]string, len(wantVolumes))
	for _, name := range wantVolumes {
		volumeSources[name] = filepath.Join(current, "volumes", name)
	}
	if err := docker.RestoreManagedVolumes(ctx, spec, volumeSources, manifest.Generation); err != nil {
		return errors.Join(err, transaction.Rollback())
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("restored data is active but old filesystem generations need manual cleanup: %w", err)
	}
	fmt.Fprintf(os.Stdout, "restored operational generation %s; workspace compute remains offline\nrollback generation %s retained at %s; use 'fern backup rollback' if post-restore validation fails\n", manifest.Generation, rollbackGeneration, rollbackRoot)
	return nil
}

func runBackupRollback(args []string, log *slog.Logger) error {
	fs, options := backupFlags("backup rollback", "Activate the durable pre-restore generation after a failed restore validation.")
	recoveryDirectory := fs.String("recovery-dir", "", "staged generation and durable rollback directory")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if options.stateDirectory == "" {
		return errors.New("cannot determine Fern state directory")
	}
	if *recoveryDirectory == "" {
		*recoveryDirectory = filepath.Join(options.stateDirectory, "recovery")
	}
	cfg, spec, err := loadBackupSpec(*options)
	if err != nil {
		return err
	}
	rollbackRoot := filepath.Join(*recoveryDirectory, operationalRollbackDirectory)
	manifest, paths, volumeSources, err := readOperationalRollback(rollbackRoot, options.stateDirectory, options.configPath, options.envPath, cfg.Workspace.Repo, fernruntime.ManagedVolumeNames(spec))
	if err != nil {
		return err
	}
	lease, err := registry.Acquire(filepath.Join(options.stateDirectory, "locks"), spec.Name)
	if err != nil {
		return fmt.Errorf("rollback requires the offline workspace lease: %w", err)
	}
	defer lease.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	docker, err := openBackupDocker(log)
	if err != nil {
		return err
	}
	defer docker.Close()
	if err := docker.Destroy(ctx, spec.Name); err != nil {
		return fmt.Errorf("quiesce workspace compute: %w", err)
	}
	transaction, err := prepareFilesystemRestorePaths(paths, manifest.Generation)
	if err != nil {
		return err
	}
	if err := transaction.Activate(); err != nil {
		transaction.Cleanup()
		return err
	}
	if err := checkpointSQLiteTree(ctx, options.stateDirectory); err != nil {
		return errors.Join(fmt.Errorf("validate rollback filesystem generation: %w", err), transaction.Rollback())
	}
	if err := docker.RestoreManagedVolumes(ctx, spec, volumeSources, manifest.Generation); err != nil {
		return errors.Join(err, transaction.Rollback())
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("rollback data is active but replaced filesystem paths need manual cleanup: %w", err)
	}
	fmt.Fprintf(os.Stdout, "rolled back to durable operational generation %s; workspace compute remains offline\nrollback material retained at %s\n", manifest.Generation, rollbackRoot)
	return nil
}

func loadBackupSpec(options backupOptions) (config.Config, fernruntime.Spec, error) {
	return loadUpSpec(upOptions{configPath: options.configPath, envPath: options.envPath, configRequired: true})
}

func newBackupGeneration() (string, error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate backup identifier: %w", err)
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random), nil
}

func checkpointSQLiteTree(ctx context.Context, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() && path != root {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if excludedStateEntries[strings.Split(relative, string(filepath.Separator))[0]] {
				return filepath.SkipDir
			}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected in Fern state: %s", path)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(path))
		if extension != ".db" && extension != ".sqlite" && extension != ".sqlite3" {
			return nil
		}
		dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
		query := dsn.Query()
		query.Set("mode", "rw")
		query.Add("_pragma", "busy_timeout(5000)")
		dsn.RawQuery = query.Encode()
		database, err := sql.Open("sqlite", dsn.String())
		if err != nil {
			return fmt.Errorf("open SQLite state %q: %w", path, err)
		}
		var busy, logFrames, checkpointed int
		err = database.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
		if err == nil && busy != 0 {
			err = fmt.Errorf("SQLite checkpoint remained busy")
		}
		if err == nil {
			var integrity string
			err = database.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity)
			if err == nil && integrity != "ok" {
				err = fmt.Errorf("SQLite integrity_check returned %q", integrity)
			}
		}
		err = errors.Join(err, database.Close())
		if err != nil {
			return fmt.Errorf("checkpoint SQLite state %q: %w", path, err)
		}
		return nil
	})
}

var excludedStateEntries = map[string]bool{
	"locks": true, "recovery": true, "backup-operator": true,
}

func stageFernState(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read Fern state: %w", err)
	}
	for _, entry := range entries {
		if excludedStateEntries[entry.Name()] {
			continue
		}
		if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func stageConfig(configPath, envPath, destination string) error {
	if filepath.Base(configPath) == filepath.Base(envPath) {
		return errors.New("configuration and environment files must have distinct names")
	}
	if err := copyPath(configPath, filepath.Join(destination, filepath.Base(configPath))); err != nil {
		return err
	}
	return copyPath(envPath, filepath.Join(destination, filepath.Base(envPath)))
}

func ensureBackupEpoch(stateDirectory string) (string, string, error) {
	lockDirectory := filepath.Join(stateDirectory, "backup-operator")
	if err := os.MkdirAll(lockDirectory, 0o700); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(lockDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", "", errors.New("backup operator directory must be a private real directory")
	}
	marker := filepath.Join(lockDirectory, "appliance-epoch")
	data, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		value, generationErr := newBackupGeneration()
		if generationErr != nil {
			return "", "", generationErr
		}
		file, createErr := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			_, createErr = io.WriteString(file, value+"\n")
			createErr = errors.Join(createErr, file.Sync(), file.Close())
		}
		if createErr != nil && !os.IsExist(createErr) {
			return "", "", createErr
		}
		data, err = os.ReadFile(marker)
	}
	if err != nil {
		return "", "", err
	}
	epoch := strings.TrimSpace(string(data))
	if epoch == "" || strings.ContainsAny(epoch, " /\\\t\r\n") {
		return "", "", errors.New("invalid backup appliance epoch")
	}
	return epoch, lockDirectory, nil
}

func runBackupArchiveTool(ctx context.Context, override string, args []string, stdout, stderr io.Writer) error {
	path := override
	var cleanup func()
	if path == "" {
		file, err := os.CreateTemp("", ".fern-host-backup-*.py")
		if err != nil {
			return err
		}
		path = file.Name()
		cleanup = func() { _ = os.Remove(path) }
		defer cleanup()
		if err := file.Chmod(0o700); err != nil {
			file.Close()
			return err
		}
		if _, err := file.Write(backupscript.HostBackupTool); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	command := exec.CommandContext(ctx, "python3", append([]string{path}, args...)...)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("host backup archive utility: %w", err)
	}
	return nil
}

type stagedBackupManifest struct {
	Generation   string   `json:"generation"`
	NamedVolumes []string `json:"named_volumes"`
	Credentials  struct {
		Policy string `json:"policy"`
	} `json:"credentials"`
}

func credentialInputForRestore(bundle, requested string) (string, error) {
	manifest, err := readStagedBackupManifest(filepath.Join(bundle, "BACKUP-MANIFEST.json"))
	if err != nil {
		return "", fmt.Errorf("read backup credential policy: %w", err)
	}
	switch manifest.Credentials.Policy {
	case "external":
		if requested == "" {
			requested = bundle + ".credentials.tar"
		}
		return requested, nil
	case "exclude":
		return requested, nil
	default:
		return "", errors.New("backup manifest has an invalid credential policy")
	}
}

func readStagedBackupManifest(path string) (stagedBackupManifest, error) {
	var manifest stagedBackupManifest
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Generation == "" {
		return manifest, errors.New("staged backup manifest has no generation")
	}
	return manifest, nil
}

type restorePath struct {
	source, target string
}

type operationalRollbackManifest struct {
	SchemaVersion      int                       `json:"schema_version"`
	Generation         string                    `json:"generation"`
	RestoredGeneration string                    `json:"restored_generation"`
	Paths              []operationalRollbackPath `json:"paths"`
	NamedVolumes       []string                  `json:"named_volumes"`
}

type operationalRollbackPath struct {
	Target    string `json:"target"`
	HadTarget bool   `json:"had_target"`
}

type preparedRestorePath struct {
	target, staged, prior string
	hadTarget             bool
	activated             bool
}

type filesystemRestore struct {
	paths []preparedRestorePath
}

func prepareFilesystemRestore(current, stateDirectory, configPath, envPath, repository, generation string) (*filesystemRestore, error) {
	paths := []restorePath{
		{source: filepath.Join(current, "config", filepath.Base(configPath)), target: configPath},
		{source: filepath.Join(current, "config", filepath.Base(envPath)), target: envPath},
		{source: filepath.Join(current, "repository"), target: repository},
	}
	if _, err := os.Lstat(paths[1].source); os.IsNotExist(err) {
		paths[1].source = ""
	}
	stagedState := filepath.Join(current, "state")
	stateEntries, err := os.ReadDir(stagedState)
	if err != nil {
		return nil, err
	}
	currentEntries, err := os.ReadDir(stateDirectory)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for _, entries := range [][]os.DirEntry{stateEntries, currentEntries} {
		for _, entry := range entries {
			if !excludedStateEntries[entry.Name()] {
				names[entry.Name()] = true
			}
		}
	}
	for name := range names {
		source := filepath.Join(stagedState, name)
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			source = ""
		}
		paths = append(paths, restorePath{source: source, target: filepath.Join(stateDirectory, name)})
	}
	slices.SortFunc(paths[3:], func(left, right restorePath) int { return strings.Compare(left.target, right.target) })
	for index := range paths {
		paths[index].target, err = filepath.Abs(paths[index].target)
		if err != nil {
			return nil, err
		}
	}
	for left := range paths {
		for right := left + 1; right < len(paths); right++ {
			if pathContains(paths[left].target, paths[right].target) || pathContains(paths[right].target, paths[left].target) {
				return nil, fmt.Errorf("restore destinations overlap: %s and %s", paths[left].target, paths[right].target)
			}
		}
	}
	return prepareFilesystemRestorePaths(paths, generation)
}

func prepareFilesystemRestorePaths(paths []restorePath, generation string) (*filesystemRestore, error) {
	transaction := &filesystemRestore{}
	for index, path := range paths {
		item := preparedRestorePath{target: path.target}
		suffix := fmt.Sprintf(".fern-%s-%d", generation, index)
		item.staged = path.target + suffix + ".staged"
		item.prior = path.target + suffix + ".prior"
		if _, stagedErr := os.Lstat(item.staged); stagedErr == nil || !os.IsNotExist(stagedErr) {
			transaction.Cleanup()
			return nil, fmt.Errorf("restore staging path already exists for %s", path.target)
		}
		if _, priorErr := os.Lstat(item.prior); priorErr == nil || !os.IsNotExist(priorErr) {
			transaction.Cleanup()
			return nil, fmt.Errorf("restore rollback path already exists for %s", path.target)
		}
		transaction.paths = append(transaction.paths, item)
		if path.source != "" {
			if err := copyPath(path.source, item.staged); err != nil {
				transaction.Cleanup()
				return nil, err
			}
		}
	}
	return transaction, nil
}

func createOperationalRollback(ctx context.Context, docker backupDocker, spec fernruntime.Spec, recoveryDirectory, generation, restoredGeneration string, paths []preparedRestorePath) (string, error) {
	root := filepath.Join(recoveryDirectory, operationalRollbackDirectory)
	staging := root + ".staged"
	if pathExists(root) || pathExists(staging) {
		return "", fmt.Errorf("durable operational rollback generation already exists at %s", root)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	filesystem := filepath.Join(staging, "filesystem")
	volumes := filepath.Join(staging, "volumes")
	if err := os.Mkdir(filesystem, 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(volumes, 0o700); err != nil {
		return "", err
	}
	manifest := operationalRollbackManifest{SchemaVersion: 1, Generation: generation, RestoredGeneration: restoredGeneration}
	for index, path := range paths {
		item := operationalRollbackPath{Target: path.target, HadTarget: pathExists(path.target)}
		manifest.Paths = append(manifest.Paths, item)
		if item.HadTarget {
			if err := copyPath(path.target, filepath.Join(filesystem, fmt.Sprintf("%d", index))); err != nil {
				return "", fmt.Errorf("snapshot rollback path %s: %w", path.target, err)
			}
		}
	}
	volumeNames, err := docker.ExportManagedVolumes(ctx, spec, volumes)
	if err != nil {
		return "", fmt.Errorf("snapshot durable Docker rollback generation: %w", err)
	}
	manifest.NamedVolumes = volumeNames
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	encoded = append(encoded, '\n')
	manifestPath := filepath.Join(staging, "ROLLBACK-MANIFEST.json")
	file, err := os.OpenFile(manifestPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return "", err
	}
	if err := os.Rename(staging, root); err != nil {
		return "", err
	}
	committed = true
	return root, nil
}

func readOperationalRollback(root, stateDirectory, configPath, envPath, repository string, wantVolumes []string) (operationalRollbackManifest, []restorePath, map[string]string, error) {
	var manifest operationalRollbackManifest
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return manifest, nil, nil, fmt.Errorf("durable operational rollback generation is unavailable or unsafe at %s", root)
	}
	data, err := os.ReadFile(filepath.Join(root, "ROLLBACK-MANIFEST.json"))
	if err != nil {
		return manifest, nil, nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || manifest.SchemaVersion != 1 || manifest.Generation == "" || manifest.RestoredGeneration == "" {
		return manifest, nil, nil, errors.New("invalid durable operational rollback manifest")
	}
	configTarget, _ := filepath.Abs(configPath)
	envTarget, _ := filepath.Abs(envPath)
	repositoryTarget, _ := filepath.Abs(repository)
	stateTarget, _ := filepath.Abs(stateDirectory)
	if len(manifest.Paths) < 3 || manifest.Paths[0].Target != configTarget || manifest.Paths[1].Target != envTarget || manifest.Paths[2].Target != repositoryTarget || !slices.Equal(manifest.NamedVolumes, wantVolumes) {
		return manifest, nil, nil, errors.New("rollback generation does not match the active Fern configuration")
	}
	paths := make([]restorePath, 0, len(manifest.Paths))
	for index, item := range manifest.Paths {
		if index >= 3 {
			parent, err := filepath.Rel(stateTarget, item.Target)
			if err != nil || parent == "." || filepath.Dir(parent) != "." || excludedStateEntries[filepath.Base(item.Target)] {
				return manifest, nil, nil, errors.New("rollback manifest contains an invalid Fern state target")
			}
		}
		source := ""
		if item.HadTarget {
			source = filepath.Join(root, "filesystem", fmt.Sprintf("%d", index))
			if !pathExists(source) {
				return manifest, nil, nil, errors.New("rollback filesystem generation is incomplete")
			}
		}
		paths = append(paths, restorePath{source: source, target: item.Target})
	}
	volumeSources := make(map[string]string, len(wantVolumes))
	for _, name := range wantVolumes {
		source := filepath.Join(root, "volumes", name)
		if !pathExists(source) {
			return manifest, nil, nil, errors.New("rollback Docker generation is incomplete")
		}
		volumeSources[name] = source
	}
	return manifest, paths, volumeSources, nil
}

func (transaction *filesystemRestore) Activate() error {
	for index := range transaction.paths {
		item := &transaction.paths[index]
		if info, err := os.Lstat(item.target); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.Join(fmt.Errorf("restore target is a symlink: %s", item.target), transaction.Rollback())
			}
			if err := os.Rename(item.target, item.prior); err != nil {
				return errors.Join(err, transaction.Rollback())
			}
			item.hadTarget = true
		} else if !os.IsNotExist(err) {
			return errors.Join(err, transaction.Rollback())
		}
		if pathExists(item.staged) {
			if err := os.Rename(item.staged, item.target); err != nil {
				return errors.Join(err, transaction.Rollback())
			}
		}
		item.activated = true
	}
	return nil
}

func (transaction *filesystemRestore) Rollback() error {
	var result error
	for index := len(transaction.paths) - 1; index >= 0; index-- {
		item := &transaction.paths[index]
		if item.activated && pathExists(item.target) {
			result = errors.Join(result, os.RemoveAll(item.target))
		}
		if item.hadTarget && pathExists(item.prior) {
			result = errors.Join(result, os.Rename(item.prior, item.target))
		}
		if pathExists(item.staged) {
			result = errors.Join(result, os.RemoveAll(item.staged))
		}
	}
	return result
}

func (transaction *filesystemRestore) Commit() error {
	var result error
	for _, item := range transaction.paths {
		if pathExists(item.prior) {
			result = errors.Join(result, os.RemoveAll(item.prior))
		}
	}
	return result
}

func (transaction *filesystemRestore) Cleanup() {
	for _, item := range transaction.paths {
		if pathExists(item.staged) {
			_ = os.RemoveAll(item.staged)
		}
	}
}

func copyPath(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf("link or special backup source rejected: %s", source)
	}
	if info.Mode().IsRegular() {
		if details, ok := info.Sys().(*syscall.Stat_t); ok && details.Nlink != 1 {
			return fmt.Errorf("hard-linked backup source rejected: %s", source)
		}
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, output.Sync(), output.Close())
	}
	if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
