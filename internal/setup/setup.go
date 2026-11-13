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
	"maps"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
	"sigs.k8s.io/yaml"
)

// version changes when what goes into a golden snapshot does, so goldens
// built the old way aren't used.
const version = "fern-golden-2"

// Contract files: what a golden build runs. Lockfiles: what it installs.
// Both are read at the repository's root.
var (
	contractFiles = slices.Concat([]string{
		"services.yaml",
		".agents/setup", ".agents/prepare", ".fern/setup", ".fern/prepare",
	}, ToolFiles)
	lockFiles = []string{
		"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb",
		"go.sum", "Cargo.lock", "uv.lock", "poetry.lock", "requirements.txt", "Gemfile.lock", "composer.lock",
	}
)

// ToolFiles are mise's files that pin a repository's tools, at its root.
// The environment's box installs what they pin into ToolsDir (fern-init),
// so a golden snapshot holds the tools and every thread shares them.
var ToolFiles = []string{"mise.toml", ".mise.toml", ".tool-versions", ".config/mise.toml", ".config/mise/config.toml"}

// ToolsDir is where a workspace's tools are installed: mise's data folder.
// The watch skips it, as all of .fern/.
const ToolsDir = ".fern/tools"

// GuessedTools is the mise config of a Guess, in ToolsDir: fern-init
// writes it there and points mise at it, outside the repository's files.
const GuessedTools = ToolsDir + "/guess.toml"

// ToolsKey returns a hash of the tool files in dir, or of the tools a Guess
// gives a repository without them, or "" if there are none: the tools
// installed in ToolsDir are up to date when it is unchanged.
func ToolsKey(dir string) (string, error) {
	h := sha256.New()
	has, err := hasToolFiles(dir)
	if err != nil {
		return "", err
	}
	if !has {
		g, err := MakeGuess(dir)
		if err != nil || g.Tools == nil {
			return "", err
		}
		sum := sha256.Sum256(g.Tools)
		return hex.EncodeToString(sum[:]), nil
	}
	for _, p := range ToolFiles {
		sum, err := fileSum(dir, p)
		if err != nil {
			return "", fmt.Errorf("tools: %s: %w", p, err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", p, sum) // a hash takes every write
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hasToolFiles reports whether dir pins tools of its own.
func hasToolFiles(dir string) (bool, error) {
	for _, p := range ToolFiles {
		sum, err := fileSum(dir, p)
		if err != nil {
			return false, fmt.Errorf("tools: %s: %w", p, err)
		}
		if sum != "missing" {
			return true, nil
		}
	}
	return false, nil
}

// maxFile bounds a file read into the key: a workspace is the agent's, and
// a key mustn't cost the daemon much.
const maxFile = 16 << 20

// ErrNoContract means the repository has nothing a golden snapshot would
// hold: no services.yaml, no setup script, no pinned tools and nothing
// MakeGuess could run.
var ErrNoContract = errors.New("no setup contract")

// Key returns the golden key of the repository checked out at dir, for
// boxes from image: a hash of its contract files, its lockfiles, the files
// services.yaml lists under golden.inputs (such as a seed), the files a
// Guess read, image and the format version. Two checkouts with the same key
// can share a golden snapshot.
func Key(dir, image string) (string, error) {
	paths := slices.Concat(contractFiles, lockFiles)
	has := false
	for _, p := range contractFiles {
		if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			has = true
		}
	}
	g, err := MakeGuess(dir)
	if err != nil {
		return "", err
	}
	if g.Services != nil {
		has = true
		paths = append(paths, g.Inputs...)
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
		return nil, nil // a guess has none
	}
	if err != nil {
		return nil, err
	}
	f, err := parseServices(data, false)
	if err != nil {
		return nil, err
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

// ServicesFile returns dir's services.yaml, or the one MakeGuess makes
// when it has none, and whether it is guessed; nil if there is neither.
func ServicesFile(dir string) (data []byte, guessed bool, err error) {
	data, err = readFile(dir, "services.yaml")
	if !errors.Is(err, fs.ErrNotExist) {
		return data, false, err
	}
	g, err := MakeGuess(dir)
	return g.Services, g.Services != nil, err
}

// ServicesYAML is a repository's services.yaml: what its box runs, what its
// golden snapshot depends on and what its box asks to be given.
type ServicesYAML struct {
	Services map[string]Service `json:"services"`
	// Golden lists, under inputs, the files a golden build depends on beyond
	// the contract's and the lockfiles, such as a seed (Key).
	Golden struct {
		Inputs []string `json:"inputs,omitempty"`
	} `json:"golden"`
	Limits Limits `json:"limits"`
}

// Service is one entry under services: in services.yaml. Its commands are
// shell scripts, which fern-init runs with /bin/sh -c in the workspace.
type Service struct {
	Run   string `json:"run"`
	Stop  string `json:"stop,omitempty"`  // graceful stop; else SIGTERM
	Ready string `json:"ready,omitempty"` // exits 0 when the service is ready
	// Listen is where the service accepts connections, "unix:PATH" or
	// "tcp:PORT": fern-init holds it and starts the service on demand, at
	// its first connection. Without it, the service starts with the box.
	Listen string `json:"listen,omitempty"`
	// Port is the port the service serves on, which it gets as PORT. The
	// first service with one, by name, is the environment's default
	// preview (PreviewPort).
	Port int `json:"port,omitempty"`
}

// ParseServices parses a services.yaml, data, for the box that is to run
// it (fern-init): a field it doesn't know, or one given twice, is an error,
// so a box never runs with part of its file left out, such as a ready check
// under a misspelt name.
func ParseServices(data []byte) (ServicesYAML, error) { return parseServices(data, true) }

// parseServices is the one parser of services.yaml, for its two kinds of
// reader. Both refuse a file that isn't YAML or has a field of the wrong
// type, and they differ in one thing:
//
//   - strict, the box's (ParseServices): a field it doesn't know is an
//     error;
//   - not strict, the daemon's (Key's golden inputs, AskedLimits and
//     PreviewPort): a field it doesn't know is passed over. Each takes its
//     own part of a file the box judges as a whole, and the box's refusal is
//     what reaches the thread: a wake that fails with the field's name.
func parseServices(data []byte, strict bool) (ServicesYAML, error) {
	unmarshal := yaml.Unmarshal
	if strict {
		unmarshal = yaml.UnmarshalStrict
	}
	var f ServicesYAML
	if err := unmarshal(data, &f); err != nil {
		return ServicesYAML{}, fmt.Errorf("services.yaml: %w", err)
	}
	return f, nil
}

// Limits are what a repository's services.yaml asks for its box, under
// limits:. A field left out, or 0, asks for the node's default.
type Limits struct {
	CPUs   float64 `json:"cpus"`
	Memory int64   `json:"memory"` // MiB
	Pids   int64   `json:"pids"`
}

// AskedLimits returns the limits dir's services.yaml asks for; none without
// the file, as a guessed one asks for none. The file is the agent's, read
// as root: readFile keeps it to dir, and the node caps what it may ask.
func AskedLimits(dir string) (Limits, error) {
	data, err := readFile(dir, "services.yaml")
	if errors.Is(err, fs.ErrNotExist) {
		return Limits{}, nil
	}
	if err != nil {
		return Limits{}, err
	}
	f, err := parseServices(data, false)
	return f.Limits, err
}

// PreviewPort returns the port of the first service in dir's services.yaml
// (or its guess), by name, that has one: the environment's default
// preview. 0 means none. The file is the agent's, read as root: readFile
// keeps it to dir.
func PreviewPort(dir string) (int, error) {
	data, _, err := ServicesFile(dir)
	if err != nil || data == nil {
		return 0, err
	}
	f, err := parseServices(data, false)
	if err != nil {
		return 0, err
	}
	for _, name := range slices.Sorted(maps.Keys(f.Services)) {
		if p := f.Services[name].Port; p > 0 && p <= 65535 {
			return p, nil
		}
	}
	return 0, nil
}

// setupScripts are where a repository keeps its setup script, the first that
// exists: a file in .fern/ replaces the .agents/ one (architecture §8).
var setupScripts = []string{".fern/setup", ".agents/setup"}

// Script returns the path, in the repository checked out at dir, of its
// setup script: what a golden build runs once, after the tools are installed
// and before the services' first start, so that every thread starts with
// what it installs and builds. "" means it has none. Only a regular file
// counts: the script runs in a box, but the workspace is the agent's, and
// the daemon follows no link of its own to learn of it.
func Script(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	for _, p := range setupScripts {
		fi, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) || err == nil && !fi.Mode().IsRegular() {
			continue
		}
		if err != nil {
			return "", err
		}
		return p, nil
	}
	return "", nil
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
