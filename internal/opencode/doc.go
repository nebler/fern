// Package opencode is Fern's boundary to the OpenCode server inside a run:
// the narrow authenticated client the coordinator drives, and the Router that
// exposes a constrained attachment surface for exactly one live runtime. Both
// are pinned to domain.SourceProfile; compatibility with that one source
// profile is deliberate rather than a promise to accept newer response shapes.
//
// # Client
//
// The client owns bounded, strict wire decoding and evidence interpretation.
// The coordinator owns durable intent and retry policy, and the Docker
// provider supplies the endpoint and credentials; nothing here inspects
// Docker, writes store state, or decides that a run succeeded.
// ParseTrustedOrigin is pure syntax validation used by config, not a
// reachability or TLS check.
//
// The *Once methods send at most one mutation with no retry and no
// request-body replay; a transport failure after dispatch is ambiguous and
// never authorizes replaying a prompt. Reconciliation is evidence-only:
// exhausting a history scan bound is uncertainty (ErrScanBound), never proof
// of absence, and unknown durable event types fail closed. ObservePending
// reports WorkUnknown unless it has positive evidence, and its three reads are
// not an atomic snapshot. Only exact typed 404/409 bodies count as not-found
// or conflict evidence. Error values never carry response bodies or secrets.
//
// # Router
//
// Router owns the one fixed loopback listener for the serial run lane and its
// binding to exactly one OpenCode runtime. It issues short-lived attachment
// capabilities and proxies a constrained OpenCode surface. It is neither a
// generic reverse proxy nor a durable routing database: bindings and
// capabilities are process-local and do not survive restart. TLS and private
// ingress are supplied by composition; the runtime transport is injected by
// the Docker provider.
//
// A binding is the full RouteIdentity (run tuple, session, container ID, start
// time, token, epoch), not just an endpoint, so a restarted process is a new
// runtime. Removal has two stages: Remove unpublishes, cancels admitted
// requests, and drains, leaving a reuse fence even if draining times out;
// ConfirmRemoval clears it once drained. The coordinator removes routes only
// for a run that is durably cleaning and so never activates that route again.
// IssueAttachment mints access only for the durable tuple naming the active
// runtime.
//
// Attachment is not read-only: owned-session prompting and selected
// question/permission replies are allowed, while foreign sessions, /fern,
// WebSockets, encoded paths, and workspaces outside /home/user/workspace are
// denied. Client credentials and cookies are stripped before proxying. Session
// lists are projected to the attached session, and SSE frames naming a foreign
// session are dropped; this is a profile-specific filter, not an
// information-flow proof. Path checks are lexical and do not resolve symlinks.
package opencode
