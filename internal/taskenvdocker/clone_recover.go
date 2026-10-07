package taskenvdocker

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/nebler/fern/internal/safeio"
)

type cloneRecoveryKind uint8

const (
	cloneRecoveryStage cloneRecoveryKind = iota + 1
	cloneRecoveryQuarantine
)

type cloneRecoveryLocation struct {
	kind   cloneRecoveryKind
	path   string
	parent string
}

func (p *Provider) findRecoverableClones(ctx context.Context, marker cloneMarker) ([]cloneRecoveryLocation, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return nil, false, err
	}
	var locations []cloneRecoveryLocation
	unknown := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		path := filepath.Join(p.root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, false, err
		}
		if sameCloneIdentity(info, marker) {
			if validRecoveryName(entry.Name(), ".clone-quarantine-") && entry.IsDir() {
				locations = append(locations, cloneRecoveryLocation{kind: cloneRecoveryQuarantine, path: path, parent: p.root})
			} else if entry.Name() != marker.Clone {
				unknown = true
			}
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		candidate := filepath.Join(path, "clone")
		candidateInfo, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if !sameCloneIdentity(candidateInfo, marker) {
			continue
		}
		if validRecoveryName(entry.Name(), ".clone-stage-") && candidateInfo.IsDir() && candidateInfo.Mode()&os.ModeSymlink == 0 {
			locations = append(locations, cloneRecoveryLocation{kind: cloneRecoveryStage, path: candidate, parent: path})
		} else {
			unknown = true
		}
	}
	known := make(map[string]bool, len(locations))
	for _, location := range locations {
		known[location.path] = true
	}
	err = filepath.WalkDir(p.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() || path == p.root {
			return nil
		}
		if known[path] {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if sameCloneIdentity(info, marker) {
			unknown = true
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return locations, unknown, nil
}

func sameCloneIdentity(info os.FileInfo, marker cloneMarker) bool {
	device, inode, err := safeio.Identity(info)
	return err == nil && device == marker.Device && inode == marker.Inode
}

// validRecoveryName accepts only names this package generates: prefix plus
// one crypto/rand.Text value.
func validRecoveryName(name, prefix string) bool {
	suffix, ok := strings.CutPrefix(name, prefix)
	return ok && len(suffix) == 26 && strings.Trim(suffix, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") == ""
}
