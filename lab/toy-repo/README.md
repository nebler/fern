# toy-repo

The lab's second sample, with no database: a notes web app in plain Node, no
dependencies. Notes live in `data/notes.json` in the environment, so a revert
must bring them back from Fern's snapshot, not from git.

- `services.yaml` runs the app on :3000.
- `curl localhost:3000/notes` lists the notes; `POST /notes` with
  `{"text": "..."}` adds one.
- `node --test` runs the tests.
