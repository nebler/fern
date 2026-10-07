// Package runapi is the plugin-authenticated run HTTP boundary at
// /fern/api/runs. It owns routing (one method/scope table), scope checks, strict wire DTOs, response and
// error projection, the committed create/stop/seal commands (service.go), and
// the configured-checkout Git base verifier (gitverifier.go). Durable SQL
// authority lives in store.
//
// Ingress authenticates; this package is not authentication middleware. It
// accepts OpenCode plugin actors whose identity matches the ingress bearer
// authorization, and the operator actor for discovery and attachment only
// (list, get, attach) with workspace-wide visibility. A scope never grants
// access to another plugin's runs: the store still filters plugins by owner.
//
// The list's attachable flag is advisory, not a reservation. Attachment
// requires durable readiness and then a successful issuance from the
// opencode Router, which owns credential expiry and runtime fencing.
// The returned credentials are secrets: responses are no-store and nothing
// here retains or logs them.
//
// Request idempotency hashes are SHA-256 over the command kind, a newline, and
// the private v1 typed-struct encoding. HTTP DTOs are converted to a private
// intent first so wire changes cannot silently alter idempotency identity; do
// not replace this with raw-JSON or map hashing. Create checks replay before
// base verification, so a valid prior acceptance still replays after its base
// becomes unreachable. Authority mismatch is hidden as
// not-found and takes precedence over hash conflict. Wake runs only after a
// successful, non-replayed commit.
//
// Result reads verify retention once per immutable (result, bundle digest)
// tuple and cache only successes; a verifier failure is reported as unverified
// retention, not as an error. The Git verifier proves
// the base is a commit reachable from HEAD or an origin tracking ref without
// fetching or mutating refs.
package runapi
