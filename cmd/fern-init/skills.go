package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// cloudflareSkills is where the box image keeps the skills Claude gets in a
// repository that deploys to Cloudflare: Cloudflare's own and fern-deploy
// (images/Dockerfile).
const cloudflareSkills = "/usr/local/share/fern/skills/cloudflare"

// linkSkills gives Claude the skills in from, or takes them away: a link in
// Claude's folder (claudeDir/skills, outside the workspace) to each skill
// folder in from, which is in the box's image. Links are cheap and follow
// the image, so an update brings new skills. A skill someone else put in
// Claude's folder, of the same name or not, is left alone; a link of
// Fern's to a skill the image no longer has is removed.
func linkSkills(claudeDir, from string, on bool) error {
	dst := filepath.Join(claudeDir, "skills")
	var want []string
	if on {
		entries, err := os.ReadDir(from)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				want = append(want, e.Name())
			}
		}
	}
	if len(want) > 0 {
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
	}
	for _, name := range want {
		link := filepath.Join(dst, name)
		if _, err := os.Lstat(link); err == nil {
			continue // ours from an earlier start, or someone's own
		}
		if err := os.Symlink(filepath.Join(from, name), link); err != nil {
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
		if err != nil || !strings.HasPrefix(target, from+"/") {
			continue // not a link of Fern's
		}
		if _, err := os.Stat(target); on && err == nil {
			continue
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return nil
}
