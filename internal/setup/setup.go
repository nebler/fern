// Package setup reads a repository's setup contract (architecture §8) and
// computes its golden key: what a golden snapshot was built from.
package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
	"sigs.k8s.io/yaml"
)

// version changes when what goes into a golden snapshot does, so goldens
// built the old way aren't used.
const version = "fern-golden-1"

// Contract files: what a golden build runs. Lockfiles: what it installs.
// Both are read at the repository's root.
var (
	contractFiles = []string{
		"services.yaml",
		".agents/setup", ".agents/prepare", ".fern/setup", ".fern/prepare",
	}
	lockFiles = []string{
		"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb",
		"go.sum", "Cargo.lock", "uv.lock", "poetry.lock", "requirements.txt", "Gemfile.lock", "composer.lock",
	}
)

// maxFile bounds a file read into the key: a workspace is the agent's, and
// a key mustn't cost the daemon much.
const maxFile = 16 << 20

// ErrNoContract means the repository has nothing a golden snapshot would
// hold: no services.yaml and no setup script.
var ErrNoContract = errors.New("no setup contract")

// Key returns the golden key of the repository checked out at dir, for
// boxes from image: a hash of its contract files, its lockfiles, the files
// services.yaml lists under golden.inputs (such as a seed), image and the
// format version. Two checkouts with the same key can share a golden
// snapshot.
func Key(dir, image string) (string, error) {
	paths := slices.Concat(contractFiles, lockFiles)
	has := false
	for _, p := range contractFiles {
		if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			has = true
		}
	}
	if !has {
		return "", ErrNoContract
	}
	inputs, err := goldenInputs(dir)
	if err != nil {
		return "", err
	}
	paths = append(paths, inputs...)
	slices.Sort(paths)
	paths = slices.Compact(paths)

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00", version, image) // a hash takes every write
	for _, p := range paths {
		sum, err := fileSum(dir, p)
		if err != nil {
			return "", fmt.Errorf("golden key: %s: %w", p, err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", p, sum)
	}
	return "golden-" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

// goldenInputs returns the paths services.yaml lists under golden.inputs,
// cleaned and inside dir.
func goldenInputs(dir string) ([]string, error) {
	data, err := readFile(dir, "services.yaml")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Golden struct {
			Inputs []string `json:"inputs"`
		} `json:"golden"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("services.yaml: %w", err)
	}
	var paths []string
	for _, p := range f.Golden.Inputs {
		clean := filepath.Clean(p)
		if !filepath.IsLocal(clean) {
			return nil, fmt.Errorf("services.yaml: golden input %q is outside the repository", p)
		}
		paths = append(paths, clean)
	}
	return paths, nil
}

// fileSum returns the SHA-256 of file p in dir, or "missing".
func fileSum(dir, p string) (string, error) {
	data, err := readFile(dir, p)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// readFile reads regular file p in dir, at most maxFile bytes: a workspace
// could point a path at a device, a pipe or a file outside it.
func readFile(dir, p string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// The root keeps symlinks from leading out of dir.
	f, err := root.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink", p)
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if fi.Size() > maxFile {
		return nil, fmt.Errorf("%s is over %d MiB", p, maxFile>>20)
	}
	return io.ReadAll(io.LimitReader(f, maxFile))
}

// builderPrefix starts the IDs of the environments that build goldens.
const builderPrefix = "golden-"

// IsBuilder reports whether environment id builds golden snapshots. Its box
// gets no credentials: what it makes goes into every thread of its
// repository.
func IsBuilder(id string) bool { return strings.HasPrefix(id, builderPrefix) }

// sourceSuffix ends the ID of the environment that holds a checkout sent
// ahead of a repository's first worktree, until its golden build has copied
// it. An ID from ID ends in hex, so the suffix can't be part of one.
const sourceSuffix = "-src"

// SourceID returns the ID of the environment that holds repo's checkout for
// a golden build ahead of its first worktree.
func SourceID(repo string) string { return ID(repo) + sourceSuffix }

// BuilderOf returns the ID of the builder whose source environment id is,
// from SourceID, or false if id isn't one.
func BuilderOf(id string) (string, bool) {
	if !IsBuilder(id) {
		return "", false
	}
	return strings.CutSuffix(id, sourceSuffix)
}

// ID returns the ID of the environment that builds repo's golden snapshots.
func ID(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	name := strings.ToLower(repo)
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	// A readable part and a hash, so that two names that read the same
	// (My_App, my-app) stay apart, within a DNS label's 63 characters.
	readable := strings.Trim(b.String(), "-")
	if len(readable) > 30 {
		readable = strings.Trim(readable[:30], "-")
	}
	if readable == "" {
		readable = "repo"
	}
	return builderPrefix + readable + "-" + hex.EncodeToString(sum[:])[:8]
}
