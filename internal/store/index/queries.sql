-- name: Env :one
SELECT * FROM environments WHERE id = ?;

-- name: Envs :many
SELECT * FROM environments ORDER BY id;

-- name: EnvAt :one
SELECT * FROM environments WHERE workspace = ?;

-- name: InsertEnv :exec
INSERT INTO environments (id, workspace, git, created_at, box_token) VALUES (?, ?, ?, ?, ?);

-- name: SetEnvGit :exec
UPDATE environments SET git = ? WHERE id = ?;

-- name: DeleteEnv :exec
DELETE FROM environments WHERE id = ?;

-- KeepSnapshot adds a snapshot's row unless it has one.
-- name: KeepSnapshot :exec
INSERT OR IGNORE INTO snapshots (env, id, created_at) VALUES (?, ?, ?);

-- ReplaceSnapshot adds a snapshot's row, replacing one of a snapshot that
-- is gone.
-- name: ReplaceSnapshot :exec
INSERT OR REPLACE INTO snapshots (env, id, created_at) VALUES (?, ?, ?);

-- name: DeleteSnapshot :exec
DELETE FROM snapshots WHERE env = ? AND id = ?;

-- Snapshots lists an environment's snapshots, oldest first.
-- name: Snapshots :many
SELECT id, created_at FROM snapshots WHERE env = ? ORDER BY created_at, rowid;

-- name: HasSnapshot :one
SELECT EXISTS (SELECT 1 FROM snapshots WHERE env = ? AND id = ?);

-- SnapshotsBefore lists an environment's snapshots whose ID is LIKE
-- pattern, taken before before, except keep.
-- name: SnapshotsBefore :many
SELECT id FROM snapshots
WHERE env = sqlc.arg(env) AND id LIKE sqlc.arg(pattern) AND id != sqlc.arg(keep) AND created_at < sqlc.arg(before);

-- SnapshotsBeyond lists an environment's snapshots whose ID isn't LIKE
-- pattern, newest first, after the newest keep.
-- name: SnapshotsBeyond :many
SELECT id FROM snapshots
WHERE env = sqlc.arg(env) AND id NOT LIKE sqlc.arg(pattern)
ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET sqlc.arg(keep);

-- EnvsWithSnapshot lists the environments that have a snapshot named id,
-- oldest first.
-- name: EnvsWithSnapshot :many
SELECT env FROM snapshots WHERE id = ? ORDER BY created_at, rowid;
