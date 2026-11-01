// Package backup ships a node's threads to an S3-compatible object store,
// encrypted, and brings them back on a new node: each environment's
// workspace as btrfs send streams, incremental after the first, and the
// state around them (the index, T3's data, the projects' repositories and
// Claude's folders) as an archive. A manifest in the bucket says what makes
// up the latest backup; it is written last, so a backup that stops leaves
// only objects and snapshots the next one removes.
package backup

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// Config is where backups go, read from a file of NAME=VALUE lines
// (/etc/fern/backup.env, root only).
type Config struct {
	S3  S3
	Key []byte // encrypts the backups; kept off the node too, to restore
}

// LoadConfig reads the config at path, or returns nil if there is none.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	vals := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && !strings.HasPrefix(k, "#") {
			vals[k] = strings.Trim(v, `"'`)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	key, err := ParseKey(vals["FERN_BACKUP_KEY"])
	if err != nil {
		return nil, fmt.Errorf("%s: FERN_BACKUP_KEY: %w", path, err)
	}
	c := &Config{Key: key, S3: S3{
		Endpoint: vals["FERN_BACKUP_ENDPOINT"], Region: vals["FERN_BACKUP_REGION"], Bucket: vals["FERN_BACKUP_BUCKET"],
		AccessKey: vals["FERN_BACKUP_ACCESS_KEY"], SecretKey: vals["FERN_BACKUP_SECRET_KEY"],
		HC: &http.Client{Timeout: 30 * time.Minute},
	}}
	required := map[string]string{
		"ENDPOINT": c.S3.Endpoint, "REGION": c.S3.Region, "BUCKET": c.S3.Bucket,
		"ACCESS_KEY": c.S3.AccessKey, "SECRET_KEY": c.S3.SecretKey,
	}
	for name, v := range required {
		if v == "" {
			return nil, fmt.Errorf("%s: FERN_BACKUP_%s is missing", path, name)
		}
	}
	return c, nil
}

// Manifest is what makes up a node's latest backup.
type Manifest struct {
	Time  time.Time            `json:"time"`
	State string               `json:"state"` // the state archive's key
	Envs  map[string]EnvBackup `json:"envs"`
}

// EnvBackup is an environment's workspace: a full send, then incremental
// ones, each from the one before.
type EnvBackup struct {
	// Workspace is relative to the pool.
	Workspace string `json:"workspace"`
	// Generation is the btrfs generation the last sent snapshot was taken at.
	Generation uint64 `json:"generation"`
	Chain      []Send `json:"chain"`
}

// Send is one btrfs send stream of a backup snapshot.
type Send struct {
	Snapshot string `json:"snapshot"`
	Key      string `json:"key"`
	Size     int64  `json:"size"`
}

// Envs makes the snapshots backups send, through the environments' actors.
type Envs interface {
	Snapshot(ctx context.Context, id, snapID string, replace bool) error
	DeleteSnapshot(ctx context.Context, id, snapID string) error
}

// Store reads the environments and their snapshots.
type Store interface {
	List(ctx context.Context) ([]store.Env, error)
	Snapshots(ctx context.Context, id string) ([]store.Snapshot, error)
	SnapshotPath(id, snapID string) string
	BackupIndex(ctx context.Context, path string) error
}

// snapPrefix starts the names of the snapshots backups take; only the last
// one sent is kept, as the next send's parent.
const snapPrefix = "fern-backup-"

// maxChain is how many incremental sends follow a full one before the next
// is full again: a restore receives them all.
const maxChain = 7 * 24

// Backer backs up the node's environments to cfg's bucket, under
// fern/<node>/.
type Backer struct {
	cfg    *Config
	prefix string
	pool   string
	store  Store
	envs   Envs
	log    *slog.Logger
}

// New returns a Backer of node's pool.
func New(cfg *Config, node, pool string, st Store, envs Envs, log *slog.Logger) *Backer {
	return &Backer{cfg: cfg, prefix: nodePrefix(node), pool: pool, store: st, envs: envs, log: log}
}

// Run backs up every interval until ctx is done.
func (b *Backer) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		start := time.Now()
		if err := b.Once(ctx); err != nil {
			b.log.WarnContext(ctx, "backup failed", slog.Any("error", err))
		} else {
			b.log.InfoContext(ctx, "backed up", slog.Duration("took", time.Since(start)))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once makes a backup now.
func (b *Backer) Once(ctx context.Context) error {
	m, err := b.manifest(ctx)
	if err != nil {
		return err
	}
	envs, err := b.store.List(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]bool)
	var errs []error
	for _, env := range envs {
		// A golden builder's work is built again from its repository.
		if setup.IsBuilder(env.ID) {
			continue
		}
		live[env.ID] = true
		eb, err := b.env(ctx, env, m.Envs[env.ID])
		if err != nil {
			errs = append(errs, fmt.Errorf("back up %s: %w", env.ID, err))
			continue
		}
		m.Envs[env.ID] = eb
		if err := b.save(ctx, m); err != nil {
			return err
		}
	}
	for id := range m.Envs {
		if !live[id] {
			delete(m.Envs, id)
		}
	}
	state, err := b.state(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	m.State, m.Time = state, time.Now().UTC()
	if err := b.save(ctx, m); err != nil {
		return err
	}
	errs = append(errs, b.sweep(ctx, m))
	return errors.Join(errs...)
}

// env sends environment env's workspace if it changed since eb, its last
// backup, and returns its backup now.
func (b *Backer) env(ctx context.Context, env store.Env, eb EnvBackup) (EnvBackup, error) {
	gen, err := generation(ctx, env.Workspace)
	if err != nil {
		return eb, err
	}
	if len(eb.Chain) > 0 && gen == eb.Generation {
		return eb, nil
	}
	snaps, err := b.store.Snapshots(ctx, env.ID)
	if err != nil {
		return eb, err
	}
	parent := ""
	if n := len(eb.Chain); n > 0 && n < maxChain {
		parent = eb.Chain[n-1].Snapshot
	}
	// Leftovers of a backup that stopped, and a parent pruned since.
	hasParent := false
	for _, s := range snaps {
		switch {
		case !strings.HasPrefix(s.ID, snapPrefix):
		case s.ID == parent:
			hasParent = true
		default:
			if err := b.envs.DeleteSnapshot(ctx, env.ID, s.ID); err != nil {
				return eb, err
			}
		}
	}
	if !hasParent {
		parent = ""
	}

	name := snapPrefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := b.envs.Snapshot(ctx, env.ID, name, false); err != nil {
		return eb, err
	}
	key := b.prefix + "envs/" + env.ID + "/" + name
	size, err := b.send(ctx, env.ID, name, parent, key)
	if err != nil {
		_ = b.envs.DeleteSnapshot(ctx, env.ID, name)
		return eb, err
	}
	// The snapshot moved the workspace's generation to its own: a write
	// since moves it past.
	taken, err := createdAt(ctx, b.store.SnapshotPath(env.ID, name))
	if err != nil {
		return eb, err
	}
	send := Send{Snapshot: name, Key: key, Size: size}
	if parent == "" {
		eb.Chain = []Send{send} // the old chain's objects go with the sweep
	} else {
		eb.Chain = append(eb.Chain, send)
		if err := b.envs.DeleteSnapshot(ctx, env.ID, parent); err != nil {
			b.log.WarnContext(ctx, "deleting a backup's parent snapshot failed", slog.String("env", env.ID), slog.Any("error", err))
		}
	}
	rel, err := filepath.Rel(b.pool, env.Workspace)
	if err != nil || !filepath.IsLocal(rel) {
		return eb, fmt.Errorf("workspace %s isn't in the pool", env.Workspace)
	}
	eb.Workspace, eb.Generation = rel, taken
	b.log.InfoContext(ctx, "backed up a workspace", slog.String("env", env.ID), slog.Bool("full", parent == ""), slog.Int64("bytes", size))
	return eb, nil
}

// send uploads btrfs send of environment id's snapshot name, from parent if
// it isn't "", compressed and encrypted, as key.
func (b *Backer) send(ctx context.Context, id, name, parent, key string) (int64, error) {
	args := []string{"send", "-q"}
	if parent != "" {
		args = append(args, "-p", b.store.SnapshotPath(id, parent))
	}
	// A failed upload kills the send, which would block on its full pipe.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "btrfs", append(args, b.store.SnapshotPath(id, name))...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	size, perr := b.put(ctx, key, out)
	if perr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil && perr == nil {
		return 0, fmt.Errorf("btrfs send: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return size, perr
}

// put uploads r, compressed and encrypted, as key.
func (b *Backer) put(ctx context.Context, key string, r io.Reader) (int64, error) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	// The compressor's goroutine ends when r does, or the upload stops
	// reading, which closes pr.
	go func() {
		defer close(done)
		zw := gzip.NewWriter(pw)
		_, err := io.Copy(zw, r)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	enc, err := Encrypt(b.cfg.Key, pr)
	if err != nil {
		_ = pr.Close()
		<-done
		return 0, err
	}
	size, err := b.cfg.S3.Put(ctx, key, enc)
	_ = pr.CloseWithError(errors.New("upload ended"))
	<-done
	return size, err
}

// state uploads the archive of the state around the workspaces and returns
// its key.
func (b *Backer) state(ctx context.Context) (string, error) {
	tmp, err := os.MkdirTemp("", "fern-backup-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	index := filepath.Join(tmp, "fern.sqlite")
	if err := b.store.BackupIndex(ctx, index); err != nil {
		return "", err
	}
	// T3's databases are copied as SQLite copies them, not as files that
	// may be half written.
	t3dbs, err := copyDatabases(ctx, filepath.Join(b.pool, "userdata"), tmp)
	if err != nil {
		return "", err
	}
	key := b.prefix + "state/" + strconv.FormatInt(time.Now().UnixNano(), 10)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(writeState(pw, b.pool, index, t3dbs))
	}()
	_, err = b.put(ctx, key, pr)
	_ = pr.CloseWithError(errors.New("upload ended"))
	<-done
	return key, err
}

// sweep deletes the objects under the node's prefix that m doesn't name:
// earlier chains and states, and what a backup that stopped uploaded.
func (b *Backer) sweep(ctx context.Context, m Manifest) error {
	keep := map[string]bool{b.prefix + "manifest.json": true, m.State: true}
	for _, eb := range m.Envs {
		for _, s := range eb.Chain {
			keep[s.Key] = true
		}
	}
	keys, err := b.cfg.S3.List(ctx, b.prefix)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		if !keep[k] {
			errs = append(errs, b.cfg.S3.Delete(ctx, k))
		}
	}
	return errors.Join(errs...)
}

func (b *Backer) manifest(ctx context.Context) (Manifest, error) {
	return readManifest(ctx, b.cfg, b.prefix)
}

func readManifest(ctx context.Context, cfg *Config, prefix string) (Manifest, error) {
	m := Manifest{Envs: make(map[string]EnvBackup)}
	r, err := cfg.S3.Get(ctx, prefix+"manifest.json")
	if errors.Is(err, ErrNoObject) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	defer func() { _ = r.Close() }()
	plain, err := Decrypt(cfg.Key, r)
	if err != nil {
		return m, err
	}
	if err := json.NewDecoder(plain).Decode(&m); err != nil {
		return m, fmt.Errorf("the backup's manifest: %w", err)
	}
	if m.Envs == nil {
		m.Envs = make(map[string]EnvBackup)
	}
	return m, nil
}

func (b *Backer) save(ctx context.Context, m Manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	enc, err := Encrypt(b.cfg.Key, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	_, err = b.cfg.S3.Put(ctx, b.prefix+"manifest.json", enc)
	return err
}

// transid is btrfs subvolume find-new's last line: the subvolume's current
// generation.
var transid = regexp.MustCompile(`transid marker was (\d+)`)

// generation returns subvolume dir's btrfs generation, which grows with
// every change to it.
func generation(ctx context.Context, dir string) (uint64, error) {
	out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "find-new", dir, "18446744073709551615").Output()
	if err != nil {
		return 0, fmt.Errorf("btrfs subvolume find-new %s: %w", dir, err)
	}
	m := transid.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("btrfs subvolume find-new %s: %q", dir, out)
	}
	return strconv.ParseUint(string(m[1]), 10, 64)
}

// genAtCreation is btrfs subvolume show's line with the transaction a
// snapshot was taken in.
var genAtCreation = regexp.MustCompile(`Gen at creation:\s*(\d+)`)

// createdAt returns the btrfs generation snapshot dir was taken at.
func createdAt(ctx context.Context, dir string) (uint64, error) {
	out, err := exec.CommandContext(ctx, "btrfs", "subvolume", "show", dir).Output()
	if err != nil {
		return 0, fmt.Errorf("btrfs subvolume show %s: %w", dir, err)
	}
	m := genAtCreation.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("btrfs subvolume show %s: no generation in %q", dir, out)
	}
	return strconv.ParseUint(string(m[1]), 10, 64)
}

// nodePrefix is where node's backups are in the bucket.
func nodePrefix(node string) string { return path.Join("fern", node) + "/" }
