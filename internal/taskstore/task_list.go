package taskstore

import (
	"context"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

const MaxTaskListLimit = 200

func (s *Store) ListTasks(ctx context.Context, workspaceID task.WorkspaceID, limit int) ([]TaskSnapshot, error) {
	if _, err := task.ParseWorkspaceID(string(workspaceID)); err != nil || limit < 1 || limit > MaxTaskListLimit {
		return nil, fmt.Errorf("%w: task list", ErrInvalidInput)
	}
	rows, err := s.db.QueryContext(ctx, taskSelect+` WHERE t.workspace_id=? ORDER BY t.updated_at DESC,t.id DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]Task, 0, limit)
	for rows.Next() {
		value, scanErr := scanTask(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan task list: %w", scanErr)
		}
		tasks = append(tasks, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task list: %w", err)
	}
	result := make([]TaskSnapshot, 0, len(tasks))
	for _, owner := range tasks {
		attempt, readErr := s.GetAttempt(ctx, owner.CurrentAttemptID)
		if readErr != nil {
			return nil, readErr
		}
		if attempt.TaskID != owner.ID || attempt.WorkspaceID != workspaceID {
			return nil, ErrCorruptStore
		}
		result = append(result, TaskSnapshot{Task: owner, Attempt: attempt})
	}
	return result, nil
}
