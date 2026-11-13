package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Delete removes environment id's record and disk, its workspace included.
// Deleting an environment that doesn't exist is not an error. Its box must
// be gone.
//
// It marks the environment and removes its record, so it is never listed
// without its workspace, then clears the disk the way Reconcile or the next
// Create would after a crash.
func (s *Store) Delete(ctx context.Context, id string) error {
	env, err := s.Get(ctx, id)
	switch {
	case err == nil:
		if err := s.writeMarker(id, env.Workspace, env.Group); err != nil {
			return fmt.Errorf("delete %s: %w", id, err)
		}
		if err := s.step("delete: marked"); err != nil {
			return err
		}
		if err := os.Remove(s.recordPath(id)); err != nil {
			return fmt.Errorf("delete %s: %w", id, err)
		}
		if err := s.step("delete: unrecorded"); err != nil {
			return err
		}
	case !errors.Is(err, ErrNotFound):
		return err
	}
	if err := s.clearLeftovers(ctx, id); err != nil {
		return fmt.Errorf("delete %s: %w", id, err)
	}
	return nil
}

// Reconcile finishes or clears what a crash left half-way: the creates and
// deletes that stopped, and the leftovers of restores and adopts. It prunes
// old pre-restore snapshots, and returns the environment directories it
// keeps and can't use: they hold data and have neither a record nor a
// marker, or their record can't be read.
//
// With quota groups, it puts each environment's workspace and newest
// snapshot into its group again, the only subvolumes a crash can leave
// outside it; with limits on (EnableLimits), it brings in the environments
// made without a group; and it destroys the groups nothing names
// (ClearGroups).
func (s *Store) Reconcile(ctx context.Context) (lost []string, err error) {
	dirs, err := os.ReadDir(filepath.Join(s.root, "envs"))
	if err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	for _, d := range dirs {
		id := d.Name()
		if CheckEnvID(id) != nil {
			lost = append(lost, s.envDir(id))
			continue
		}
		env, err := s.Get(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			err := s.clearLeftovers(ctx, id)
			if errors.Is(err, errUnrecorded) {
				lost = append(lost, s.envDir(id))
			} else if err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
		case errors.Is(err, errBadRecord):
			lost = append(lost, s.envDir(id))
		case err != nil:
			return lost, fmt.Errorf("reconcile: %w", err)
		default:
			// A restore that stopped left live.new. So can an adopt its
			// adopt.new, when a power cut undoes the cleanup that came before
			// the record; the copy is garbage either way. A create or delete
			// that stopped next to its record left the marker, and a record
			// or a failure's file being replaced its new copy.
			if err := s.deleteSubvolumes(ctx, s.restorePath(id), s.adoptPath(id)); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := os.RemoveAll(s.adoptPath(id)); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			for _, p := range []string{s.markerPath(id), s.recordPath(id) + ".new", s.failurePath(id) + ".new", s.snapshotFailurePath(id) + ".new"} {
				if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return lost, fmt.Errorf("reconcile %s: %w", id, err)
				}
			}
			// A golden build's BuildSource replaces its builder's live/; one
			// that stopped in between left none, and the next build makes it
			// afresh. Only that leaves an environment with a record without one.
			if _, err := os.Lstat(env.Workspace); errors.Is(err, fs.ErrNotExist) && env.Workspace == s.livePath(id) {
				if err := s.makeWorkspace(ctx, id, env.Workspace, "", env.Group); err != nil {
					return lost, fmt.Errorf("reconcile %s: %w", id, err)
				}
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := s.prunePreRestores(ctx, id, ""); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
			if err := s.reconcileGroup(ctx, env); err != nil {
				return lost, fmt.Errorf("reconcile %s: %w", id, err)
			}
		}
	}
	if _, err := s.ClearGroups(ctx); err != nil {
		return lost, fmt.Errorf("reconcile: %w", err)
	}
	return lost, nil
}

// reconcileGroup puts env's workspace and newest snapshot into its quota
// group again, or brings env in if it has none and limits are on.
func (s *Store) reconcileGroup(ctx context.Context, env Env) error {
	if env.Group == 0 {
		if s.limit == 0 {
			return nil
		}
		return s.bringIn(ctx, env)
	}
	if err := s.joinWorkspace(ctx, env.ID, env.Workspace, env.Group); err != nil {
		return err
	}
	snaps, err := s.Snapshots(ctx, env.ID)
	if err != nil || len(snaps) == 0 {
		return err
	}
	return s.join(ctx, env.ID, s.snapPath(env.ID, snaps[len(snaps)-1].ID), env.Group)
}

// clearLeftovers removes what a create or delete of environment id, which has
// no record, left when it stopped: a marked directory is garbage, and so is
// the workspace its marker names. It refuses an unmarked directory that holds
// data: then its record was lost some other way, and only a person should
// throw it away.
func (s *Store) clearLeftovers(ctx context.Context, id string) error {
	dir := s.envDir(id)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	workspace, group, err := s.readMarker(id)
	if errors.Is(err, fs.ErrNotExist) {
		kept, err := holdsData(dir)
		if err != nil {
			return err
		}
		if kept {
			return fmt.Errorf("%s: %w", dir, errUnrecorded)
		}
	} else if err != nil {
		return err
	}
	if err := s.removeDisk(ctx, id, workspace); err != nil {
		return err
	}
	if group == 0 {
		return nil
	}
	// Mostly refused at once: btrfs's cleaner hasn't dropped the deleted
	// subvolumes yet. ClearGroups destroys it later.
	return s.destroyGroup(group)
}

// holdsData reports whether an environment directory has a live/, a
// snapshot or anything in Claude's folder.
func holdsData(dir string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, "live")); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for _, sub := range []string{"snaps", "claude"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// removeDisk deletes environment id's directory and, if workspace isn't
// empty, that workspace too. Subvolumes go before their parent directories,
// so a cleanup interrupted by a crash can be repeated.
func (s *Store) removeDisk(ctx context.Context, id, workspace string) error {
	paths := []string{s.livePath(id), s.restorePath(id), s.adoptPath(id)}
	if workspace != "" && workspace != s.livePath(id) {
		paths = append(paths, workspace)
	}
	snaps, err := os.ReadDir(s.snapsDir(id))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, snap := range snaps {
		paths = append(paths, s.snapPath(id, snap.Name()))
	}
	if err := s.deleteSubvolumes(ctx, paths...); err != nil {
		return err
	}
	return os.RemoveAll(s.envDir(id))
}

// writeMarker records that a create or delete of environment id has begun,
// so that what it leaves is garbage. A delete names its workspace, which goes
// with it; a create names none: the next Create resumes a workspace at a
// caller's path. Both name the environment's quota group, if it has one,
// which goes too: the workspace on the first line, the group on the second.
func (s *Store) writeMarker(id, workspace string, group uint64) error {
	if err := os.MkdirAll(s.envDir(id), 0o755); err != nil {
		return err
	}
	data := workspace
	if group != 0 {
		data += "\n" + strconv.FormatUint(group, 10) + "\n"
	}
	return os.WriteFile(s.markerPath(id), []byte(data), 0o644)
}

// readMarker returns the workspace and quota group environment id's marker
// names, or an error that is fs.ErrNotExist if it has none. A marker of a
// Fern before quota groups is the workspace alone.
func (s *Store) readMarker(id string) (workspace string, group uint64, err error) {
	data, err := os.ReadFile(s.markerPath(id))
	if err != nil {
		return "", 0, err
	}
	first, rest, _ := strings.Cut(string(data), "\n")
	if rest = strings.TrimSpace(rest); rest != "" {
		// One a power cut cut short names no group: its environment had
		// none yet, or ClearGroups finds it.
		group, _ = strconv.ParseUint(rest, 10, 64)
	}
	return strings.TrimSpace(first), group, nil
}
