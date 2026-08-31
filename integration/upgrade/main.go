package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	_ "modernc.org/sqlite"
)

type migrationRecord struct {
	version  int
	name     string
	checksum string
}

var currentLedger = []migrationRecord{
	{1, "initial_task_store", "3ff013a514506ce2b74258c04c831178c5e0273cb50173a5a06b381939151d3d"},
	{2, "execution_projection_and_results", "68cf5e97208957d894d02fbe5254ce2f4738348c6fd8b1d2f5b2548b1a15141b"},
	{3, "verification_and_publication_journals", "88b52b43bf12184bc54e6ffde5f21ed98888ec42026164ec9f3becfeb37f46b6"},
	{4, "user_authorized_snapshot_seals", "218ec52d97faf9a95c1790230e47c22cad37d9974b2e8d9a118fe3935ebbf03b"},
	{5, "explicit_workspace_github_authority", "675011d6037df1b806e78e0a98576c43a0594d6c21a3d54e9f10fb8c4017ec8d"},
	{6, "publication_admission_receipts", "6c54a44e10e025c2d82a1466b184c74ea8d2641530472aca02e79b4cdcd301ca"},
	{7, "background_run_intents", "14adb62969106f5c6a66d12ae4e43cc6c6d31fbafe71e0f7e708cc42c2aaba2a"},
	{8, "background_run_effect_claims", "3488eb94b3bc0254b09ce5df70ed4ddc07b2b25505dfffa2fc57cbec0c2a6026"},
}

func main() {
	databasePath := flag.String("database", "", "copied baseline task database to migrate")
	flag.Parse()
	if *databasePath == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./integration/upgrade --database PATH")
		os.Exit(2)
	}
	if err := migrateAndVerify(*databasePath); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade verification failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("task store upgraded to schema %d with durable state intact\n", len(currentLedger))
}

func migrateAndVerify(path string) error {
	ctx := context.Background()
	store, err := taskstore.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("open current task store: %w", err)
	}
	workspaceID := task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	workspace, err := store.GetWorkspace(ctx, workspaceID)
	if err != nil || workspace.Name != "demo" || workspace.GitHubAuthority != taskstore.GitHubAuthorityAppBroker || workspace.InstallationID != 123 || workspace.RepositoryID != 987654321 {
		_ = store.Close()
		return fmt.Errorf("workspace semantic state differs: %+v: %v", workspace, err)
	}
	taskID := task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-00000000002a")
	durableTask, err := store.GetTask(ctx, taskID)
	if err != nil || durableTask.Prompt != "Preserve durable compatibility" || durableTask.State != task.TaskQueued || durableTask.CurrentAttemptID != "att_0198d34d-6a50-75fb-b1f2-00000000002a" {
		_ = store.Close()
		return fmt.Errorf("task semantic state differs: %+v: %v", durableTask, err)
	}
	attempt, err := store.GetAttempt(ctx, task.AttemptID("att_0198d34d-6a50-75fb-b1f2-00000000002a"))
	if err != nil || attempt.State != task.AttemptPrepared || attempt.Model != "model-1" {
		_ = store.Close()
		return fmt.Errorf("attempt semantic state differs: %+v: %v", attempt, err)
	}
	receipt, err := store.GetReceipt(ctx, task.ReceiptID("rcp_0198d34d-6a50-75fb-b1f2-00000000002a"))
	if err != nil || receipt.State != taskstore.ReceiptAccepted || receipt.IdempotencyKey != "baseline-submit" || receipt.TargetID != taskID {
		_ = store.Close()
		return fmt.Errorf("receipt semantic state differs: %+v: %v", receipt, err)
	}
	events, err := store.ListEvents(ctx, workspaceID, 0, 10)
	if err != nil || len(events.Events) != 2 || !events.CaughtUp || events.NextCursor != 2 {
		_ = store.Close()
		return fmt.Errorf("event semantic state differs: %+v: %v", events, err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close migrated task store: %w", err)
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	database, err := sql.Open("sqlite", "file:"+filepath.ToSlash(absolute)+"?mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer database.Close()
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(currentLedger) {
		return fmt.Errorf("schema version = %d, want %d: %v", version, len(currentLedger), err)
	}
	var integrity string
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("integrity_check = %q: %v", integrity, err)
	}
	foreignKeys, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	if foreignKeys.Next() {
		foreignKeys.Close()
		return fmt.Errorf("foreign_key_check found a violation")
	}
	if err := foreignKeys.Close(); err != nil {
		return err
	}
	rows, err := database.Query(`SELECT version,name,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var got migrationRecord
		if err := rows.Scan(&got.version, &got.name, &got.checksum); err != nil {
			return err
		}
		if index >= len(currentLedger) || got != currentLedger[index] {
			return fmt.Errorf("migration ledger entry %d differs: %+v", index+1, got)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if index != len(currentLedger) {
		return fmt.Errorf("migration ledger has %d entries, want %d", index, len(currentLedger))
	}
	return nil
}
