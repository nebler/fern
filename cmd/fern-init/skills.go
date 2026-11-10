package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// skillPacks is where the box image keeps the skills Claude can get, in
// packs: <pack>/<skill>/SKILL.md, and an optional <pack>/CLAUDE.md that goes
// into Claude's hint while the pack is on (images/Dockerfile).
const skillPacks = "/usr/local/share/fern/skills"

// packName is what a pack may be called: a folder in skillPacks.
var packName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// activePacks returns the packs Claude gets: the node's (FERN_SKILLS, which
// the daemon reads from /etc/fern/skills), those Fern turns on for what the
// repository is (detected), and those its services.yaml turns on, less
// those it turns off. A name that can't be a pack is dropped.
func activePacks(node string, detected, on, off []string) []string {
	var packs []string
	for _, p := range slices.Concat(strings.Fields(node), detected, on) {
		if packName.MatchString(p) && !slices.Contains(packs, p) && !slices.Contains(off, p) {
			packs = append(packs, p)
		}
	}
	return packs
}

// packNotes returns the CLAUDE.md of each pack in root that has one, for
// Claude's hint.
func packNotes(root string, packs []string) ([]string, error) {
	var notes []string
	for _, p := range packs {
		data, err := os.ReadFile(filepath.Join(root, p, "CLAUDE.md"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		notes = append(notes, strings.TrimSpace(string(data)))
	}
	return notes, nil
}

// linkSkills gives Claude the skills of packs in root, and takes away the
// others: a link in Claude's folder (claudeDir/skills, outside the
// workspace) to each skill folder, which is in the box's image. Links are
// cheap and follow the image, so an update brings new skills. Of two skills
// with one name, the first pack's wins. A skill someone else put in
// Claude's folder is left alone; a link of Fern's to a skill no active pack
// has is removed.
func linkSkills(claudeDir, root string, packs []string) error {
	dst := filepath.Join(claudeDir, "skills")
	want := make(map[string]string) // skill to its folder
	var names []string
	for _, p := range packs {
		entries, err := os.ReadDir(filepath.Join(root, p))
		if errors.Is(err, fs.ErrNotExist) {
			continue // a pack this image doesn't have
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if _, ok := want[e.Name()]; e.IsDir() && !ok {
				want[e.Name()] = filepath.Join(root, p, e.Name())
				names = append(names, e.Name())
			}
		}
	}
	if len(want) > 0 {
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		link := filepath.Join(dst, e.Name())
		target, err := os.Readlink(link)
		if err != nil || !strings.HasPrefix(target, root+"/") {
			continue // not a link of Fern's
		}
		if want[e.Name()] == target {
			continue
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	for _, name := range names {
		link := filepath.Join(dst, name)
		if _, err := os.Lstat(link); err == nil {
			continue // ours from an earlier start, or someone's own
		}
		if err := os.Symlink(want[name], link); err != nil {
			return err
		}
	}
	return nil
}
