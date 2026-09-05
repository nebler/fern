package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/taskstore"
	_ "modernc.org/sqlite"
)

const currentSchemaChecksum = "d37b38566dc707d31885b883035a43d4ef5cc829415b2a64cb91701450b231fc"

func main() {
	databasePath := flag.String("database", "", "task database to initialize or verify")
	flag.Parse()
	if *databasePath == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./integration/upgrade --database PATH")
		os.Exit(2)
	}
	if err := initializeAndVerify(*databasePath); err != nil {
		fmt.Fprintf(os.Stderr, "schema verification failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("task store schema %d initialized and verified\n", taskstore.CurrentSchemaVersion())
}

func initializeAndVerify(path string) error {
	ctx := context.Background()
	store, err := taskstore.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("open task store: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close task store: %w", err)
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
	var version, entries int
	var name, checksum, integrity string
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if err := database.QueryRow(`SELECT count(*),name,checksum FROM schema_migrations`).Scan(&entries, &name, &checksum); err != nil {
		return err
	}
	if err := database.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if version != 1 || version != taskstore.CurrentSchemaVersion() || entries != 1 || name != "initial_task_store" || checksum != currentSchemaChecksum || integrity != "ok" {
		return fmt.Errorf("version=%d current=%d entries=%d name=%q checksum=%q integrity=%q",
			version, taskstore.CurrentSchemaVersion(), entries, name, checksum, integrity)
	}
	foreignKeys, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer foreignKeys.Close()
	if foreignKeys.Next() {
		return fmt.Errorf("foreign_key_check found a violation")
	}
	return foreignKeys.Err()
}
