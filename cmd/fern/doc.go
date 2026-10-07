// Command fern is the executable composition root for Fern's disposable
// Background Run control plane. It owns command dispatch (registry.go is the
// single command table for dispatch, help, and suggestions), operator
// workflows, service wiring, and process lifetime. Durable state transitions,
// Docker ownership, artifact retention, and HTTP authorization stay in their
// internal packages; user-facing command documentation is docs/usage.md.
//
// fern up validates bootstrap configuration, binds its listeners, and takes the
// repository's host lease before assembling services. It always opens the
// workspace's one SQLite database, which backs devices and plugin
// authorization as well as runs, so those surfaces work before task services
// can be composed. A missing App installation or credentials keeps the control
// plane up with the GitHub dependency marked blocked, so run handlers report
// unavailable; bootstrap readiness is not permission to accept durable work.
// Before declaring the run profile qualified, startup re-inspects every
// retained artifact the store references. Only the serial Background Run
// coordinator runs as a worker; there is no host publication or
// result-verification worker, and the runapi base verifier only checks
// admission Git identity.
//
// Shutdown order matters: the background route is closed first, fencing
// attachment admission and its connections, before the artifact engine, Docker
// provider, and SQLite database; the host lease is released last. Offline
// backup and credential commands take the same host lease, so they require the
// server to be stopped. They stage and roll back filesystem state but promise
// no transaction spanning the host, containers, and GitHub.
package main
