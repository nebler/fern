// Package backupscript embeds the canonical host archive utility in release binaries.
package backupscript

import _ "embed"

// HostBackupTool is the same fail-closed utility shipped as a standalone release asset.
//
//go:embed fern-host-backup.py
var HostBackupTool []byte
