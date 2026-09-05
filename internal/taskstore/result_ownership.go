package taskstore

import (
	"context"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

// resultConsumerSourcePredicate requires the exact committed CAS ownership
// tuple. Callers use aliases r, t, and a.
const resultConsumerSourcePredicate = `(r.source_kind='retained_artifact' AND EXISTS (
   SELECT 1 FROM background_runs br
   JOIN retained_artifacts ra ON ra.id=r.retained_artifact_id AND ra.result_id=r.id
   JOIN background_run_exports be ON be.id=r.artifact_export_id AND be.result_id=r.id
   JOIN artifact_materializations am ON am.id=r.materialization_id AND am.result_id=r.id
   WHERE br.task_id=t.id AND br.attempt_id=a.id AND br.retained_result_id=r.id AND
    br.retained_artifact_id=ra.id AND br.artifact_export_id=be.id AND br.materialization_id=am.id AND
    br.state='result_ready' AND br.result_authority_phase IN ('artifact_committed','cleanup') AND
     be.state='completed' AND be.phase='completed' AND am.state='ready'
  ))`

// HasRetainedResultAuthority reports whether the exact committed Background Run
// and CAS ownership tuple authorizes this result for a new consumer effect.
func (s *Store) HasRetainedResultAuthority(ctx context.Context, resultID task.ResultID) (bool, error) {
	if _, err := task.ParseResultID(string(resultID)); err != nil {
		return false, fmt.Errorf("%w: result ID", ErrInvalidInput)
	}
	return hasRetainedResultAuthority(ctx, s.db, resultID)
}

func hasRetainedResultAuthority(ctx context.Context, q queryRower, resultID task.ResultID) (bool, error) {
	var authorized int
	err := q.QueryRowContext(ctx, `
SELECT EXISTS(
 SELECT 1 FROM results r
 JOIN tasks t ON t.id=r.task_id AND t.workspace_id=r.workspace_id
 JOIN attempts a ON a.id=r.attempt_id AND a.task_id=r.task_id AND a.workspace_id=r.workspace_id
 WHERE r.id=? AND `+resultConsumerSourcePredicate+`
)`, resultID).Scan(&authorized)
	if err != nil {
		return false, fmt.Errorf("inspect retained result authority: %w", err)
	}
	return authorized == 1, nil
}

func digestString(v [32]byte) string { return "sha256:" + fmt.Sprintf("%x", v[:]) }
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
