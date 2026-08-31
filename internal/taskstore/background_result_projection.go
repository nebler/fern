package taskstore

import (
	"context"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

// BackgroundRunResultProjection is an ownership-scoped immutable result view.
// CAS locators and materialization paths intentionally remain internal.
type BackgroundRunResultProjection struct {
	Run             BackgroundRun
	Result          Result
	Artifact        RetainedArtifact
	Materialization ArtifactMaterialization
}

func (s *Store) GetBackgroundRunResult(ctx context.Context, workspaceID task.WorkspaceID, taskID task.TaskID, actor task.ActorSnapshot) (BackgroundRunResultProjection, error) {
	run, err := s.GetBackgroundRun(ctx, workspaceID, taskID, actor)
	if err != nil {
		return BackgroundRunResultProjection{}, err
	}
	if run.State != BackgroundRunResultReady || run.RetainedResultID == "" || run.RetainedArtifactID == "" || run.MaterializationID == "" {
		return BackgroundRunResultProjection{}, ErrInvalidState
	}
	result, err := s.GetResult(ctx, run.RetainedResultID)
	if err != nil {
		return BackgroundRunResultProjection{}, err
	}
	artifact, err := s.GetRetainedArtifact(ctx, run.RetainedArtifactID)
	if err != nil {
		return BackgroundRunResultProjection{}, err
	}
	materialization, err := s.GetArtifactMaterialization(ctx, run.MaterializationID)
	if err != nil {
		return BackgroundRunResultProjection{}, err
	}
	if result.SourceKind != ResultSourceRetainedArtifact || result.CompletionAuthority != SealAuthorityUser ||
		result.TaskID != run.TaskID || result.AttemptID != run.AttemptID || result.RetainedArtifactID != artifact.ID ||
		artifact.ResultID != result.ID || artifact.MaterializationID != materialization.ID || materialization.ResultID != result.ID ||
		artifact.ResultCommit != result.ResultCommit || artifact.TreeOID != result.TreeOID || artifact.ChangesSHA256 != result.ManifestSHA256 ||
		artifact.ResultCommit != materialization.ResultCommit || artifact.TreeOID != materialization.TreeOID ||
		materialization.State != ArtifactMaterializationReady {
		return BackgroundRunResultProjection{}, fmt.Errorf("%w: retained result projection", ErrCorruptStore)
	}
	return BackgroundRunResultProjection{Run: run, Result: result, Artifact: artifact, Materialization: materialization}, nil
}
