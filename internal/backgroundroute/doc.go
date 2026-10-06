// Package backgroundroute owns the one fixed loopback listener for the serial
// Background Run lane and its binding to exactly one OpenCode runtime. It issues
// short-lived attachment capabilities and proxies a constrained OpenCode
// surface. It is neither a generic reverse proxy nor a durable routing
// database: bindings and capabilities are process-local and do not survive
// restart. TLS and private ingress are supplied by composition; the runtime
// transport is injected by taskenvdocker.
//
// A binding is the full runtime identity (run tuple, session, container ID,
// start time, token, epoch), not just an endpoint, so a restarted process is a
// new runtime. Removal has two stages: Remove unpublishes, cancels admitted
// requests, and drains, leaving a reuse fence even if draining times out;
// ConfirmRemoval clears it and must only be called by the coordinator after the
// durable route_removed record is committed. IssueAttachment mints access only
// for the durable tuple naming the active runtime.
//
// Attachment is not read-only: owned-session prompting and selected
// question/permission replies are allowed, while foreign sessions, /fern,
// WebSockets, encoded paths, and workspaces outside /home/user/workspace are
// denied. Client credentials and cookies are stripped before proxying. Session
// lists are projected to the attached session, and SSE frames naming a foreign
// session are dropped; this is a profile-specific filter, not an
// information-flow proof. Path checks are lexical and do not resolve symlinks.
package backgroundroute
