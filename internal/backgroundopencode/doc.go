// Package backgroundopencode is the narrow authenticated HTTP client for the
// OpenCode server inside a Background Run, pinned to domain.SourceProfile. It does
// not support the persistent workspace OpenCode API, and compatibility with that
// one source profile is deliberate rather than a promise to accept newer
// response shapes.
//
// The client owns bounded, strict wire decoding and evidence interpretation.
// The coordinator owns durable intent and retry policy, and the Docker provider
// supplies the endpoint and credentials; nothing here inspects Docker, writes
// taskstore state, or decides that a run succeeded. ParseTrustedOrigin is pure
// syntax validation used by config, not a reachability or TLS check.
//
// The *Once methods send at most one mutation with no retry and no request-body
// replay; a transport failure after dispatch is ambiguous and never authorizes
// replaying a prompt. Reconciliation is evidence-only: exhausting a history scan
// bound is uncertainty (ErrScanBound), never proof of absence, and unknown
// durable event types fail closed. ObservePending reports WorkUnknown unless it
// has positive evidence, and its three reads are not an atomic snapshot. Only
// exact typed 404/409 bodies count as not-found or conflict evidence. Error
// values never carry response bodies or secrets.
package backgroundopencode
