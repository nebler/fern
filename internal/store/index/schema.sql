CREATE TABLE IF NOT EXISTS environments (
	id         TEXT PRIMARY KEY,
	workspace  TEXT NOT NULL UNIQUE,
	git        TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	box_token  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS snapshots (
	env        TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
	id         TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (env, id)
);
