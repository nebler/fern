package taskstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const MaxTaskListLimit = 200

type taskSnapshotQueryer interface {
	queryRower
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) GetTaskSnapshot(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID) (TaskSnapshot, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil {
		return TaskSnapshot{}, fmt.Errorf("%w: workspace ID", ErrInvalidInput)
	}
	if _, err := task.ParseTaskID(string(taskID)); err != nil {
		return TaskSnapshot{}, fmt.Errorf("%w: task ID", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TaskSnapshot{}, fmt.Errorf("begin task snapshot: %w", err)
	}
	defer tx.Rollback()
	owner, err := getLegacyTask(ctx, tx, taskID)
	if err != nil {
		return TaskSnapshot{}, err
	}
	if owner.WorkspaceID != workspaceID {
		return TaskSnapshot{}, ErrNotFound
	}
	snapshot, err := readTaskSnapshot(ctx, tx, owner)
	if err != nil {
		return TaskSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskSnapshot{}, fmt.Errorf("commit task snapshot: %w", err)
	}
	return snapshot, nil
}

func (s *Store) ListTasks(ctx context.Context, workspaceID task.WorkspaceID, limit int) ([]TaskSnapshot, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || limit < 1 || limit > MaxTaskListLimit {
		return nil, fmt.Errorf("%w: task list", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin task list: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, taskSelect+` WHERE t.workspace_id=? AND NOT EXISTS
(SELECT 1 FROM background_runs br WHERE br.task_id=t.id) ORDER BY t.updated_at DESC,t.id DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	tasks := make([]Task, 0, limit)
	for rows.Next() {
		value, scanErr := scanTask(rows)
		if scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan task list: %w", scanErr)
		}
		tasks = append(tasks, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate task list: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close task list: %w", err)
	}
	result := make([]TaskSnapshot, 0, len(tasks))
	for _, owner := range tasks {
		snapshot, readErr := readTaskSnapshot(ctx, tx, owner)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, snapshot)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit task list: %w", err)
	}
	return result, nil
}

func readTaskSnapshot(ctx context.Context, q taskSnapshotQueryer, owner Task) (TaskSnapshot, error) {
	attempt, err := getAttempt(ctx, q, owner.CurrentAttemptID)
	if err != nil {
		return TaskSnapshot{}, err
	}
	if attempt.TaskID != owner.ID || attempt.WorkspaceID != owner.WorkspaceID {
		return TaskSnapshot{}, ErrCorruptStore
	}
	snapshot := TaskSnapshot{Task: owner, Attempt: attempt, Verifications: []Verification{}}
	sealRequest, err := scanSealRequest(q.QueryRowContext(ctx, sealRequestSelect+` WHERE q.task_id=? ORDER BY q.accepted_at DESC,q.id DESC LIMIT 1`, owner.ID))
	if err == nil {
		if sealRequest.WorkspaceID != owner.WorkspaceID || sealRequest.TaskID != owner.ID {
			return TaskSnapshot{}, ErrCorruptStore
		}
		snapshot.SealRequest = &sealRequest
	} else if !errors.Is(err, sql.ErrNoRows) {
		return TaskSnapshot{}, fmt.Errorf("read latest seal request: %w", err)
	}
	if owner.SealedResultID == "" {
		return snapshot, nil
	}
	result, err := getResult(ctx, q, owner.SealedResultID)
	if err != nil {
		return TaskSnapshot{}, err
	}
	if result.WorkspaceID != owner.WorkspaceID || result.TaskID != owner.ID || result.AttemptID != attempt.ID {
		return TaskSnapshot{}, ErrCorruptStore
	}
	snapshot.Result = &result
	rows, err := q.QueryContext(ctx, verificationSelect+` WHERE result_id=? ORDER BY created_at,id`, result.ID)
	if err != nil {
		return TaskSnapshot{}, fmt.Errorf("list task verifications: %w", err)
	}
	for rows.Next() {
		verification, scanErr := scanVerification(rows)
		if scanErr != nil {
			rows.Close()
			return TaskSnapshot{}, fmt.Errorf("scan task verification: %w", scanErr)
		}
		if verification.WorkspaceID != owner.WorkspaceID || verification.TaskID != owner.ID || verification.AttemptID != attempt.ID || verification.ResultID != result.ID {
			rows.Close()
			return TaskSnapshot{}, ErrCorruptStore
		}
		snapshot.Verifications = append(snapshot.Verifications, verification)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return TaskSnapshot{}, fmt.Errorf("iterate task verifications: %w", err)
	}
	if err := rows.Close(); err != nil {
		return TaskSnapshot{}, fmt.Errorf("close task verifications: %w", err)
	}
	publication, err := scanPublication(q.QueryRowContext(ctx, publicationSelect+` WHERE p.result_id=?`, result.ID))
	if err == nil {
		if publication.WorkspaceID != owner.WorkspaceID || publication.TaskID != owner.ID || publication.AttemptID != attempt.ID || publication.ResultID != result.ID {
			return TaskSnapshot{}, ErrCorruptStore
		}
		snapshot.Publication = &publication
	} else if !errors.Is(err, sql.ErrNoRows) {
		return TaskSnapshot{}, fmt.Errorf("read task publication: %w", err)
	}
	return snapshot, nil
}
