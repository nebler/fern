// Package image unpacks container images, as `docker save` writes them,
// into directories that boxes and T3's server use as their root.
//
// An image is <dir>/<id>/, its root filesystem in root/ and its config in
// config.json, where id is the config's SHA-256, as containerd's image IDs
// are. Each tag is a symlink <dir>/<tag> to its image's directory.
package image

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Image is an unpacked image.
type Image struct {
	ID   string   // the SHA-256 of its config, in hex
	Root string   // its root filesystem
	Env  []string // its environment, NAME=VALUE
}

// manifest is one image of manifest.json, which docker save writes.
type manifest struct {
	Config   string
	RepoTags []string
	Layers   []string
}

// config is the part of an image's config Fern uses.
type config struct {
	Config struct {
		Env []string `json:"Env"`
	} `json:"config"`
}

// Leftovers of a Load that stopped start with these; the next Load removes them.
const (
	stagePrefix  = ".load-"
	unpackPrefix = ".unpack-"
)

// Load unpacks every image in r, a tar stream as docker save writes it, into
// dir and points their tags at them. When a tag moves, it keeps the tag's
// previous image, which running boxes may still use, and removes older
// images no tag points to. A Load that stops leaves only temporary directories, which the
// next one removes, and tags pointing where they did.
func Load(ctx context.Context, r io.Reader, dir string) ([]Image, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := clearLeftovers(dir); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(dir, stagePrefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := extract(ctx, r, stage); err != nil {
		return nil, fmt.Errorf("read image stream: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("read image stream: %w", err)
	}
	var ms []manifest
	if err := json.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	keep := map[string]bool{}
	moved := false
	var out []Image
	for _, m := range ms {
		img, err := unpack(ctx, stage, dir, m)
		if err != nil {
			return nil, err
		}
		keep[img.ID] = true
		for _, tag := range m.RepoTags {
			prev, err := os.Readlink(filepath.Join(dir, tagName(tag)))
			if err == nil && prev == img.ID {
				continue
			}
			if err == nil {
				keep[prev] = true
			}
			if err := link(dir, img.ID, tagName(tag)); err != nil {
				return nil, err
			}
			moved = true
		}
		out = append(out, img)
	}
	if !moved {
		return out, nil // what each tag's previous image was is unknown here
	}
	return out, prune(dir, keep)
}

// Open returns the image tag points to in dir.
func Open(dir, tag string) (Image, error) {
	id, err := os.Readlink(filepath.Join(dir, tagName(tag)))
	if err != nil {
		return Image{}, fmt.Errorf("image %s: %w", tag, err)
	}
	return read(dir, id)
}

// read returns image id in dir.
func read(dir, id string) (Image, error) {
	data, err := os.ReadFile(filepath.Join(dir, id, "config.json"))
	if err != nil {
		return Image{}, fmt.Errorf("image %s: %w", id, err)
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return Image{}, fmt.Errorf("image %s: config: %w", id, err)
	}
	return Image{ID: id, Root: filepath.Join(dir, id, "root"), Env: c.Config.Env}, nil
}

// tagName is the name of tag's symlink: a repository's slashes would make
// folders.
func tagName(tag string) string { return strings.ReplaceAll(tag, "/", "_") }

// extract writes the regular files of tar stream r into stage.
func extract(ctx context.Context, r io.Reader, stage string) error {
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name, ok := clean(h.Name)
		if !ok || h.Typeflag != tar.TypeReg {
			continue
		}
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return err
		}
		if err := writeFile(root, name, tr, 0o644); err != nil {
			return err
		}
	}
}

// unpack makes image m, whose files are in stage, in dir, unless it is
// there already, and returns it.
func unpack(ctx context.Context, stage, dir string, m manifest) (Image, error) {
	cfgName, ok := clean(m.Config)
	if !ok {
		return Image{}, fmt.Errorf("manifest.json: bad config path %q", m.Config)
	}
	cfg, err := os.ReadFile(filepath.Join(stage, cfgName))
	if err != nil {
		return Image{}, fmt.Errorf("image config: %w", err)
	}
	sum := sha256.Sum256(cfg)
	id := hex.EncodeToString(sum[:])
	if img, err := read(dir, id); err == nil {
		return img, nil
	}
	tmp, err := os.MkdirTemp(dir, unpackPrefix)
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	rootDir := filepath.Join(tmp, "root")
	if err := os.Mkdir(rootDir, 0o755); err != nil {
		return Image{}, err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = root.Close() }()
	for _, l := range m.Layers {
		name, ok := clean(l)
		if !ok {
			return Image{}, fmt.Errorf("manifest.json: bad layer path %q", l)
		}
		if err := applyLayer(ctx, root, filepath.Join(stage, name)); err != nil {
			return Image{}, fmt.Errorf("image %s: layer %s: %w", id, l, err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "config.json"), cfg, 0o644); err != nil {
		return Image{}, err
	}
	// Its directory appears whole or not at all.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return Image{}, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, id)); err != nil {
		return Image{}, err
	}
	return read(dir, id)
}

// applyLayer applies the layer at file, a tar stream, possibly gzipped, to
// root: its files over what is there, less what its whiteouts remove.
func applyLayer(ctx context.Context, root *os.Root, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	// What this layer made, which an opaque whiteout in its folder keeps.
	made := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name, ok := clean(h.Name)
		if !ok {
			continue
		}
		parent, base := path.Dir(name), path.Base(name)
		switch {
		case base == ".wh..wh..opq":
			if err := clearDir(root, parent, made); err != nil {
				return err
			}
			continue
		case strings.HasPrefix(base, ".wh."):
			if err := root.RemoveAll(path.Join(parent, strings.TrimPrefix(base, ".wh."))); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		if err := applyEntry(root, name, h, tr); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		made[name] = true
	}
}

// applyEntry makes the file h describes at name, replacing what is there
// unless both are folders.
func applyEntry(root *os.Root, name string, h *tar.Header, r io.Reader) error {
	mode := h.FileInfo().Mode()
	if fi, err := root.Lstat(name); err == nil && (!fi.IsDir() || h.Typeflag != tar.TypeDir) {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err := root.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	case tar.TypeReg:
		if err := writeFile(root, name, r, 0o600); err != nil {
			return err
		}
	case tar.TypeSymlink:
		return lchown(root.Symlink(h.Linkname, name), root, name, h)
	case tar.TypeLink:
		target, ok := clean(h.Linkname)
		if !ok {
			return fmt.Errorf("bad hard link target %q", h.Linkname)
		}
		return root.Link(target, name)
	default:
		return nil // devices and FIFOs: images for boxes have none
	}
	if err := lchown(nil, root, name, h); err != nil {
		return err
	}
	// After chown, which clears setuid bits.
	if err := root.Chmod(name, mode&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	return root.Chtimes(name, h.ModTime, h.ModTime)
}

// lchown gives name h's owner, unless err, which it returns, isn't nil.
func lchown(err error, root *os.Root, name string, h *tar.Header) error {
	if err != nil {
		return err
	}
	return root.Lchown(name, h.Uid, h.Gid)
}

// clearDir removes what dir holds that this layer didn't make.
func clearDir(root *os.Root, dir string, made map[string]bool) error {
	d, err := root.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		p := path.Join(dir, n)
		if made[p] {
			continue
		}
		if err := root.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes what r holds to a new file name in root.
func writeFile(root *os.Root, name string, r io.Reader, perm fs.FileMode) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// clean returns name as a path relative to an image's root, or false if it
// is the root itself or leaves it.
func clean(name string) (string, bool) {
	p := strings.TrimPrefix(path.Clean("/"+name), "/")
	return p, p != ""
}

// link points tag at image id, replacing what it pointed to in one step.
func link(dir, id, tag string) error {
	tmp := filepath.Join(dir, stagePrefix+"link-"+rand.Text())
	if err := os.Symlink(id, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, tag)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// prune removes the images in dir that aren't in keep and no tag points to.
func prune(dir string, keep map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		if id, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
			keep[id] = true
		}
	}
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] || !isID(e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// isID reports whether name is an image's ID: 64 hex digits.
func isID(name string) bool {
	_, err := hex.DecodeString(name)
	return len(name) == 64 && err == nil
}

// clearLeftovers removes what a Load that stopped left in dir.
func clearLeftovers(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if slices.ContainsFunc([]string{stagePrefix, unpackPrefix}, func(p string) bool { return strings.HasPrefix(e.Name(), p) }) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
