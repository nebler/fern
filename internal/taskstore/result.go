package taskstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nebler/fern/internal/domain"
)

type ResultState string

// A result row is inserted when the export selects its snapshot and becomes
// sealed, exactly once, in the transaction that releases the run's resources.
const (
	ResultSelected ResultState = "selected"
	ResultSealed   ResultState = "sealed"
)

// Result is a run's retained artifact: the selected snapshot tuple, the
// canonical artifact manifest stored whole, and, once sealed, the
// materialization proof. Selected fields are immutable from insertion and the
// whole row is immutable once sealed.
type Result struct {
	ID                    domain.ResultID
	RunID                 domain.RunID
	State                 ResultState
	Outcome               domain.ResultOutcome
	BaseSHA               domain.GitOID
	ResultCommit          domain.GitOID
	TreeOID               domain.GitOID
	ChangeCount           int
	ChangesSHA256         [32]byte
	Manifest              json.RawMessage
	ManifestSHA256        [32]byte
	BundleSHA256          [32]byte
	BundleBytes           int64
	CollectedAt           time.Time
	MaterializationSHA256 [32]byte
	SealedAt              *time.Time
}

// CASLocator is the content address of the retained artifact manifest.
func (r Result) CASLocator() string { return "sha256:" + hex.EncodeToString(r.ManifestSHA256[:]) }

// SelectBackgroundRunSnapshotParams is the exact snapshot tuple taken under
// the writer fence. The outcome follows from the commit and the run's base.
type SelectBackgroundRunSnapshotParams struct {
	BackgroundRunRef
	ResultCommit           domain.GitOID
	TreeOID                domain.GitOID
	ChangeCount            int
	ChangesSHA256          [32]byte
	ArtifactManifest       json.RawMessage
	ArtifactManifestSHA256 [32]byte
	BundleSHA256           [32]byte
	BundleBytes            int64
	CollectedAt            time.Time
}

type CommitBackgroundRunRetainedResultParams struct {
	BackgroundRunRef
	// MaterializationProof binds a verified detached checkout of the installed
	// artifact; it is committed with the result, not before it.
	MaterializationProof [32]byte
}

type BackgroundRunRetainedResult struct {
	Run    BackgroundRun
	Result Result
}

// BackgroundRunResultProjection is an ownership-scoped immutable result view.
// CAS locators and materialization paths intentionally remain internal.
type BackgroundRunResultProjection struct {
	Run    BackgroundRun
	Result Result
}

const resultSelect = `SELECT id,run_id,state,outcome,base_sha,result_commit,tree_oid,change_count,changes_sha256,
manifest_json,manifest_sha256,bundle_sha256,bundle_size,collected_at,materialization_sha256,sealed_at FROM results`

// GetResult reads a selected or sealed result. A sealed run's result ID names
// its row before selection, so ErrNotFound means nothing is selected yet.
func (s *Store) GetResult(ctx context.Context, id domain.ResultID) (Result, error) {
	return getResult(ctx, s.db, id)
}

func getResult(ctx context.Context, q queryRower, id domain.ResultID) (Result, error) {
	var r Result
	var manifest string
	var changes, manifestHash, bundle, materialization []byte
	var collected int64
	var sealed sql.NullInt64
	err := q.QueryRowContext(ctx, resultSelect+` WHERE id=?`, id).Scan(&r.ID, &r.RunID, &r.State, &r.Outcome, &r.BaseSHA,
		&r.ResultCommit, &r.TreeOID, &r.ChangeCount, &changes, &manifest, &manifestHash, &bundle, &r.BundleBytes, &collected,
		&materialization, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("read result: %w", err)
	}
	r.Manifest = json.RawMessage(manifest)
	copy(r.ChangesSHA256[:], changes)
	copy(r.ManifestSHA256[:], manifestHash)
	copy(r.BundleSHA256[:], bundle)
	copy(r.MaterializationSHA256[:], materialization)
	r.CollectedAt = fromUnixMillis(collected)
	r.SealedAt = nullableTime(sealed)
	return r, nil
}

// SelectBackgroundRunSnapshot durably selects the snapshot tuple of a sealing
// run under its writer fence. Every later export pass re-derives the snapshot
// and must equal this selection; a repeated identical selection is a replay.
// The artifact manifest is agent-derived content, so it is checked here.
func (s *Store) SelectBackgroundRunSnapshot(ctx context.Context, p SelectBackgroundRunSnapshotParams) (_ Result, err error) {
	if p.ExpectedState != domain.Canceling || p.ExpectedPhase != domain.Sealing ||
		sha256.Sum256(p.ArtifactManifest) != p.ArtifactManifestSHA256 || !safeArtifactManifest(p.ArtifactManifest) ||
		validExactTimestamp(p.CollectedAt) != nil || p.ChangeCount < 0 {
		return Result{}, fmt.Errorf("%w: selected background snapshot", ErrInvalidInput)
	}
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	defer rollback(tx, &err)
	run, err := readRun(ctx, tx, p.WorkspaceID, p.RunID)
	if err != nil {
		return Result{}, err
	}
	if !p.BackgroundRunRef.matches(run) || run.Seal == nil || run.WriterFence == nil {
		return Result{}, ErrInvalidState
	}
	outcome := domain.ResultChanged
	if p.ResultCommit == run.BaseOID {
		outcome = domain.ResultNoChanges
	}
	if existing, getErr := getResult(ctx, tx, run.Seal.ResultID); getErr == nil {
		if !p.selects(existing) {
			return Result{}, ErrInvalidState
		}
		return existing, tx.Commit()
	} else if !errors.Is(getErr, ErrNotFound) {
		return Result{}, getErr
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO results(id,run_id,state,outcome,base_sha,result_commit,tree_oid,change_count,
changes_sha256,manifest_json,manifest_sha256,bundle_sha256,bundle_size,collected_at) VALUES(?,?,'selected',?,?,?,?,?,?,?,?,?,?,?)`,
		run.Seal.ResultID, run.RunID, outcome, run.BaseOID, p.ResultCommit, p.TreeOID, p.ChangeCount, p.ChangesSHA256[:],
		string(p.ArtifactManifest), p.ArtifactManifestSHA256[:], p.BundleSHA256[:], p.BundleBytes, unixMillis(p.CollectedAt)); err != nil {
		return Result{}, fmt.Errorf("select background snapshot: %w", err)
	}
	selected, err := getResult(ctx, tx, run.Seal.ResultID)
	if err != nil {
		return Result{}, err
	}
	return selected, tx.Commit()
}

// selects reports whether an already selected result is this exact snapshot,
// so a repeated export pass is idempotent.
func (p SelectBackgroundRunSnapshotParams) selects(existing Result) bool {
	return existing.ResultCommit == p.ResultCommit &&
		existing.TreeOID == p.TreeOID &&
		existing.ChangeCount == p.ChangeCount &&
		existing.ChangesSHA256 == p.ChangesSHA256 &&
		existing.ManifestSHA256 == p.ArtifactManifestSHA256 &&
		existing.BundleSHA256 == p.BundleSHA256 &&
		existing.BundleBytes == p.BundleBytes &&
		existing.CollectedAt.Equal(p.CollectedAt)
}

// MarkBackgroundRunExportRecoveryRequired records why the last export pass
// failed. The run stays sealing; the next pass retries from the selection.
func (s *Store) MarkBackgroundRunExportRecoveryRequired(ctx context.Context, ref BackgroundRunRef, reason string) (BackgroundRun, error) {
	if ref.ExpectedState != domain.Canceling || ref.ExpectedPhase != domain.Sealing || !validBoundedText(reason, 1, 1000) {
		return BackgroundRun{}, fmt.Errorf("%w: export recovery reason", ErrInvalidInput)
	}
	return s.updateRun(ctx, ref, `last_error=?`, []any{reason}, "mark background export recovery")
}

// CommitBackgroundRunRetainedResult seals the selected result and moves the
// run to result_ready/cleaning in one transaction, so resources are released
// only after the retained result is durable. Caller cancellation cannot split
// it once validation has completed.
func (s *Store) CommitBackgroundRunRetainedResult(ctx context.Context, p CommitBackgroundRunRetainedResultParams) (_ BackgroundRunRetainedResult, err error) {
	if p.ExpectedState != domain.Canceling || p.ExpectedPhase != domain.Sealing ||
		p.MaterializationProof == ([32]byte{}) {
		return BackgroundRunRetainedResult{}, fmt.Errorf("%w: retained result commit", ErrInvalidInput)
	}
	ctx = context.WithoutCancel(ctx)
	tx, release, err := s.beginWrite(ctx)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	defer release()
	defer rollback(tx, &err)
	run, err := readRun(ctx, tx, p.WorkspaceID, p.RunID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if run.Seal == nil || run.WriterFence == nil {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	sealedMS := unixMillis(p.Now)
	result, err := tx.ExecContext(ctx, `UPDATE results SET state='sealed',materialization_sha256=?,sealed_at=?
WHERE id=? AND run_id=? AND state='selected'`, p.MaterializationProof[:], sealedMS, run.Seal.ResultID, run.RunID)
	if err != nil {
		return BackgroundRunRetainedResult{}, fmt.Errorf("seal retained result: %w", err)
	}
	if changed, changeErr := result.RowsAffected(); changeErr != nil || changed != 1 {
		return BackgroundRunRetainedResult{}, ErrInvalidState
	}
	// The result is now sealed, so the run may leave sealing and release its
	// resources; the cleanup gate trigger checks exactly this.
	stored, err := updateRunTx(ctx, tx, p.BackgroundRunRef, `state='result_ready',effect_phase='cleaning',last_error=NULL`, nil,
		"commit retained result")
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	sealed, err := getResult(ctx, tx, run.Seal.ResultID)
	if err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return BackgroundRunRetainedResult{}, err
	}
	return BackgroundRunRetainedResult{Run: stored, Result: sealed}, nil
}

// GetBackgroundRunResult returns a result_ready run with its sealed result
// after the same ownership-hiding check as GetBackgroundRun.
func (s *Store) GetBackgroundRunResult(ctx context.Context, workspaceID domain.WorkspaceID, runID domain.RunID, actor domain.ActorSnapshot) (BackgroundRunResultProjection, error) {
	run, err := s.GetBackgroundRun(ctx, workspaceID, runID, actor)
	if err != nil {
		return BackgroundRunResultProjection{}, err
	}
	if run.State != domain.ResultReady || run.Seal == nil {
		return BackgroundRunResultProjection{}, ErrInvalidState
	}
	result, err := s.GetResult(ctx, run.Seal.ResultID)
	if err != nil {
		return BackgroundRunResultProjection{}, err
	}
	if result.State != ResultSealed || result.RunID != run.RunID {
		return BackgroundRunResultProjection{}, fmt.Errorf("%w: retained result projection", ErrCorruptStore)
	}
	return BackgroundRunResultProjection{Run: run, Result: result}, nil
}

// ReferencedArtifactManifestSHA256 lists the CAS manifests sealed results
// retain.
func (s *Store) ReferencedArtifactManifestSHA256(ctx context.Context) ([][32]byte, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT manifest_sha256 FROM results WHERE state='sealed' ORDER BY manifest_sha256`)
	if err != nil {
		return nil, fmt.Errorf("list referenced artifact manifests: %w", err)
	}
	defer rows.Close()
	values := make([][32]byte, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var value [32]byte
		copy(value[:], raw)
		values = append(values, value)
	}
	return values, rows.Err()
}

// safeArtifactManifest rejects agent-derived manifests that carry keys naming
// host or credential authority, which must never become durable state.
func safeArtifactManifest(value json.RawMessage) bool {
	if len(value) < 2 || len(value) > 4*1024*1024 || value[0] != '{' {
		return false
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return false
	}
	forbidden := map[string]bool{"host_path": true, "remote_url": true, "prompt": true, "environment": true,
		"credential": true, "credentials": true, "cookie": true, "cookies": true, "authorization": true, "actor_auth": true,
		"opencode_output": true, "raw_output": true}
	var inspect func(any) bool
	inspect = func(node any) bool {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[strings.ToLower(key)] || !inspect(child) {
					return false
				}
			}
		case []any:
			for _, child := range typed {
				if !inspect(child) {
					return false
				}
			}
		}
		return true
	}
	return inspect(decoded)
}
