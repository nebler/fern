package main

import (
	"fmt"
	"io"
	"time"

	"github.com/nebler/fern/internal/control"
)

func runLegacyPublicationQuarantine(args []string, output io.Writer) error {
	fs := newFlagSet("debug quarantine-publications", "Quarantine unresolved retired control publication records while fern up is stopped.")
	nameFlag := fs.String("name", "", "workspace name")
	configPath := fs.String("config", "fern.yaml", "configuration file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name, err := workspaceName(fs, *nameFlag, *configPath)
	if err != nil {
		return err
	}
	lease, err := acquireWorkspaceLease(name)
	if err != nil {
		return fmt.Errorf("legacy publication quarantine requires fern up to be stopped: %w", err)
	}
	defer lease.Release()
	controlDirectory, err := statePath("control")
	if err != nil {
		return err
	}
	store, err := control.Open(controlDirectory, name)
	if err != nil {
		return err
	}
	quarantined, err := store.QuarantineLegacyPublications(time.Now())
	if err != nil {
		return fmt.Errorf("quarantine legacy publications: %w", err)
	}
	if len(quarantined) == 0 {
		_, err = fmt.Fprintln(output, "no unresolved legacy control publications")
		return err
	}
	for _, publication := range quarantined {
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\n", publication.ID, publication.OriginalState, publication.State, publication.QuarantinedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}
