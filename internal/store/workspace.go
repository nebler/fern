package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/domain"
)

const workspaceSelect = `
SELECT id,name,state,repository_path,github_authority,installation_id,repository_id,repository_full_name,
       revision,created_at,updated_at
FROM workspaces`

// EnsureWorkspace creates a workspace once or returns the existing exact
// immutable binding with the same name. The candidate ID is ignored only when
// every authority-bearing field already matches.
func (s *Store) EnsureWorkspace(ctx context.Context, desired Workspace) (Workspace, error) {
	if err := validateWorkspace(desired); err != nil {
		return Workspace{}, err
	}
	existing, err := s.GetWorkspaceByName(ctx, desired.Name)
	if err == nil {
		if !sameWorkspaceBinding(existing, desired) {
			return Workspace{}, fmt.Errorf("%w: workspace binding differs from persisted authority", ErrInvalidState)
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Workspace{}, err
	}
	if err := s.CreateWorkspace(ctx, desired); err != nil {
		// A concurrent creator may have won the unique name. Re-read and apply
		// the same exact comparison rather than interpreting SQL text.
		existing, readErr := s.GetWorkspaceByName(ctx, desired.Name)
		if readErr != nil || !sameWorkspaceBinding(existing, desired) {
			return Workspace{}, err
		}
		return existing, nil
	}
	return s.GetWorkspace(ctx, desired.ID)
}

func (s *Store) GetWorkspace(ctx context.Context, id domain.WorkspaceID) (Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx, workspaceSelect+` WHERE id=?`, id))
}

func (s *Store) GetWorkspaceByName(ctx context.Context, name string) (Workspace, error) {
	if !validBoundedText(name, 1, 200) {
		return Workspace{}, fmt.Errorf("%w: workspace name", ErrInvalidInput)
	}
	return scanWorkspace(s.db.QueryRowContext(ctx, workspaceSelect+` WHERE name=?`, name))
}

func scanWorkspace(row rowScanner) (Workspace, error) {
	var workspace Workspace
	var installationID, repositoryID int64
	var createdAt, updatedAt int64
	err := row.Scan(
		&workspace.ID, &workspace.Name, &workspace.State, &workspace.RepositoryPath,
		&workspace.GitHubAuthority, &installationID, &repositoryID, &workspace.RepositoryFullName,
		&workspace.Revision, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("read workspace: %w", err)
	}
	workspace.InstallationID = domain.InstallationID(installationID)
	workspace.RepositoryID = domain.RepositoryID(repositoryID)
	workspace.CreatedAt = fromUnixMillis(createdAt)
	workspace.UpdatedAt = fromUnixMillis(updatedAt)
	return workspace, nil
}

// sameWorkspaceBinding compares every field except the store-assigned or
// candidate-only ID, revision, and timestamps.
func sameWorkspaceBinding(existing, desired Workspace) bool {
	existing.ID, existing.Revision, existing.CreatedAt, existing.UpdatedAt = desired.ID, desired.Revision, desired.CreatedAt, desired.UpdatedAt
	return existing == desired
}

// CreateWorkspace inserts a caller-validated workspace binding. IDs and
// timestamps are supplied by the caller; this package never generates them.
func (s *Store) CreateWorkspace(ctx context.Context, w Workspace) error {
	if err := validateWorkspace(w); err != nil {
		return err
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return fmt.Errorf("begin workspace creation: %w", err)
	}
	defer release()
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.ExecContext(ctx, `
INSERT INTO workspaces(
    id,name,state,repository_path,github_authority,installation_id,repository_id,
    repository_full_name,revision,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Name, w.State, w.RepositoryPath, w.GitHubAuthority, w.InstallationID, w.RepositoryID,
		w.RepositoryFullName, 1, unixMillis(w.CreatedAt), unixMillis(w.CreatedAt))
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace creation: %w", err)
	}
	committed = true
	return nil
}

func validateWorkspace(w Workspace) error {
	if _, err := domain.ParseWorkspaceID(string(w.ID)); err != nil {
		return fmt.Errorf("%w: workspace ID: %v", ErrInvalidInput, err)
	}
	if !w.State.valid() {
		return fmt.Errorf("%w: workspace state", ErrInvalidInput)
	}
	if !validBoundedText(w.Name, 1, 200) || !validBoundedText(w.RepositoryFullName, 1, 512) {
		return fmt.Errorf("%w: workspace text", ErrInvalidInput)
	}
	if !filepath.IsAbs(w.RepositoryPath) || filepath.Clean(w.RepositoryPath) != w.RepositoryPath || len(w.RepositoryPath) > 4096 {
		return fmt.Errorf("%w: canonical repository path", ErrInvalidInput)
	}
	if !w.GitHubAuthority.valid() ||
		w.InstallationID == 0 || uint64(w.InstallationID) > math.MaxInt64 ||
		w.RepositoryID == 0 || uint64(w.RepositoryID) > math.MaxInt64 {
		return fmt.Errorf("%w: SQLite integer range", ErrInvalidInput)
	}
	if err := validTimestamp(w.CreatedAt); err != nil {
		return err
	}
	return nil
}

func validTimestamp(v time.Time) error {
	if v.IsZero() || v.UnixMilli() < 0 {
		return fmt.Errorf("%w: timestamp", ErrInvalidInput)
	}
	return nil
}

func validBoundedText(v string, min, max int) bool {
	if !utf8.ValidString(v) || len(v) < min || len(v) > max {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func unixMillis(v time.Time) int64 { return v.UnixMilli() }

func fromUnixMillis(v int64) time.Time { return time.UnixMilli(v).UTC() }

func validExactTimestamp(value time.Time) error {
	if err := validTimestamp(value); err != nil || !value.Equal(fromUnixMillis(unixMillis(value))) {
		return fmt.Errorf("%w: timestamp must be exact Unix milliseconds", ErrInvalidInput)
	}
	return nil
}
