package image_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/image"
)

// entry is a file of a tar stream.
type entry struct {
	name, data, link string
	typ              byte
	mode             int64
	uid              int
}

func tarOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
			if typ == tar.TypeDir {
				mode = 0o755
			}
		}
		h := &tar.Header{
			Name: e.name, Typeflag: typ, Mode: mode, Uid: e.uid, Gid: e.uid, Linkname: e.link,
			Size: int64(len(e.data)), ModTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func blob(data []byte) string {
	sum := sha256.Sum256(data)
	return "blobs/sha256/" + hex.EncodeToString(sum[:])
}

// saved returns a stream as docker save writes it, of one image tagged tag
// with env and layers, its manifest last as docker writes it.
func saved(t *testing.T, tag, env string, layers ...[]byte) ([]byte, string) {
	t.Helper()
	cfg := []byte(`{"config":{"Env":["` + env + `"]}}`)
	entries := []entry{{name: "blobs/", typ: tar.TypeDir}, {name: blob(cfg), data: string(cfg)}}
	var names []string
	for _, l := range layers {
		entries = append(entries, entry{name: blob(l), data: string(l)})
		names = append(names, `"`+blob(l)+`"`)
	}
	manifest := `[{"Config":"` + blob(cfg) + `","RepoTags":["` + tag + `"],"Layers":[` + join(names) + `]}]`
	entries = append(entries, entry{name: "manifest.json", data: manifest})
	sum := sha256.Sum256(cfg)
	return tarOf(t, entries...), hex.EncodeToString(sum[:])
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// tree lists root's files: path, kind and content or link target.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out = append(out, rel+" -> "+target)
		case d.IsDir():
			out = append(out, rel+"/")
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out = append(out, rel+": "+string(data))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	base := tarOf(t,
		entry{name: "etc/", typ: tar.TypeDir},
		entry{name: "etc/a", data: "old"},
		entry{name: "etc/gone", data: "deleted above"},
		entry{name: "bin/tool", data: "tool", mode: 0o4755, uid: 1000},
		entry{name: "bin/link", link: "tool", typ: tar.TypeSymlink},
		entry{name: "bin/hard", link: "bin/tool", typ: tar.TypeLink},
		entry{name: "d/x", data: "hidden above"},
		entry{name: "../../escape", data: "stays inside"},
	)
	top := gzipped(t, tarOf(t,
		entry{name: "etc/.wh.gone"},
		entry{name: "etc/a", data: "new"},
		entry{name: "d/y", data: "kept"},
		entry{name: "d/.wh..wh..opq"},
	))
	stream, id := saved(t, "fern-box:dev", "PATH=/bin", base, top)
	// A load that stopped left these.
	for _, d := range []string{".load-1", ".unpack-1"} {
		if err := os.MkdirAll(filepath.Join(dir, d, "x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	imgs, err := image.Load(t.Context(), bytes.NewReader(stream), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := image.Image{ID: id, Root: filepath.Join(dir, id, "root"), Env: []string{"PATH=/bin"}}
	if diff := cmp.Diff([]image.Image{want}, imgs); diff != "" {
		t.Errorf("Load() mismatch (-want +got):\n%s", diff)
	}
	if got, err := image.Open(dir, "fern-box:dev"); err != nil || got.ID != id {
		t.Errorf("Open() = %v, %v; want image %s", got, err, id)
	}
	wantTree := []string{
		"bin/", "bin/hard: tool", "bin/link -> tool", "bin/tool: tool",
		"d/", "d/y: kept", "escape: stays inside", "etc/", "etc/a: new",
	}
	if diff := cmp.Diff(wantTree, tree(t, want.Root)); diff != "" {
		t.Errorf("root mismatch (-want +got):\n%s", diff)
	}
	fi, err := os.Stat(filepath.Join(want.Root, "bin/tool"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&fs.ModeSetuid == 0 || fi.Sys().(*syscall.Stat_t).Uid != 1000 {
		t.Errorf("bin/tool: mode %v, owner %d; want setuid, owned by 1000", fi.Mode(), fi.Sys().(*syscall.Stat_t).Uid)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("%s holds %d entries, want the image and its tag: leftovers removed", dir, len(entries))
	}
}

// A tag's previous image stays, which running boxes may use; older ones go.
func TestLoadKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	var ids []string
	for _, v := range []string{"V=1", "V=2", "V=3", "V=3"} {
		stream, id := saved(t, "fern-box:dev", v, tarOf(t, entry{name: "f", data: v}))
		if _, err := image.Load(t.Context(), bytes.NewReader(stream), dir); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i, id := range ids[:3] {
		_, err := os.Stat(filepath.Join(dir, id))
		if exists, want := err == nil, i > 0; exists != want {
			t.Errorf("image %d exists: %v, want %v", i+1, exists, want)
		}
	}
	if img, err := image.Open(dir, "fern-box:dev"); err != nil || img.ID != ids[3] {
		t.Errorf("Open() = %v, %v; want the last image", img, err)
	}
}

func TestLoadErrors(t *testing.T) {
	stream, _ := saved(t, "x:dev", "A=1", tarOf(t, entry{name: "f"}))
	for name, r := range map[string]io.Reader{
		"not a tar":   bytes.NewReader([]byte("nope")),
		"cut short":   bytes.NewReader(stream[:len(stream)/2]),
		"no manifest": bytes.NewReader(tarOf(t, entry{name: "x", data: "y"})),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := image.Load(t.Context(), r, dir); err == nil {
				t.Error("Load() succeeded, want an error")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("%s holds %d entries after a failed load, want none", dir, len(entries))
			}
		})
	}
	if _, err := image.Open(t.TempDir(), "x:dev"); err == nil {
		t.Error("Open() of a missing tag succeeded, want an error")
	}
}
