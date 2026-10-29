// Package index is the store's SQLite index of environments and snapshots.
// Its tables are in schema.sql and its queries in queries.sql; sqlc
// generates the rest of the package from them (sqlc.yaml, lab/sqlc.sh).
package index

import _ "embed" // for Schema

// Schema creates the index's tables if they don't exist.
//
//go:embed schema.sql
var Schema string
