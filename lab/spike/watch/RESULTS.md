# Spike: file events from Claude's box to the services' box

**Question:** a dev server (Vite, Next.js) in the services' box watches the
workspace; Claude edits it from its agent box. Does the watcher see the edits?
And would one sandbox with two containers fix it?

**Answer: no, and no.** Run 2026-10-30 in the local lab (arm64, runsc
`release-20261012.0`), `watch.py MODE` for each mode, as root on the node.

| Mode | Its own edit | Claude's change, add, delete, rename | A host write |
|---|---|---|---|
| `two-sandboxes` (today) | event | none | none |
| `binds`: one sandbox, a bind in each container | event | none | none |
| `pod`: one sandbox, `share=pod` mount hint | doesn't start | | |
| `shared`: one sandbox, `share=shared` | event | none | none |

In every mode the services' container reads the new content: only the events
are missing.

`pod` fails with `mount source already has a filestore file …; repeated
submounts are not supported with overlay optimizations`. gVisor's source says
why (`runsc/boot/sentryapi/mount_hints.go`, `ShouldShareMount`): "Only support
tmpfs for now. Bind mounts require a common gofer to mount all shared
volumes." A pod-shared bind mount is also sandbox-local, so its writes would go
to an overlay, not the host: T3 and git would not see them.

**Events cross only within one mount of one container.** So the options are:

- **Touch from inside** (tested 2026-10-30 with two sandboxes): after a change
  on the host, `touch -c -r F F` in the services' box fires `change F` for a
  changed and for a new file. Deletes need something else.
- **One container for both**, with Claude as another user: real events, one
  sandbox fewer, but Claude's login is kept from the services only by file
  permissions under one gVisor kernel, and the two can't sleep apart.
- **Polling** (`fs.watchFile`, `CHOKIDAR_USEPOLLING`): sees everything, costs
  CPU on a big tree.
