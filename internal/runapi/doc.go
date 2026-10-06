// Package runapi is the plugin-authenticated Background Run HTTP boundary at
// /fern/api/runs. It owns routing, scope checks, strict wire DTOs, response and
// error projection, the committed create/stop/seal commands (service.go), and
// the configured-checkout Git base verifier (gitverifier.go). Durable SQL
// authority lives in taskstore.
//
// Only OpenCode plugin actors whose identity matches the ingress bearer
// authorization are accepted. Terminal discovery and attachment are a separate
// surface in runclientapi; neither package is authentication middleware. A
// scope never grants access to another plugin's runs: the store still filters
// by owner.
//
// Request idempotency hashes are SHA-256 over the command kind, a newline, and
// the private v1 typed-struct encoding. HTTP DTOs are converted to a private
// intent first so wire changes cannot silently alter idempotency identity; do
// not replace this with raw-JSON or map hashing. Create checks replay before
// profile availability or base verification, so a valid prior acceptance still
// replays while execution is unavailable. Authority mismatch is hidden as
// not-found and takes precedence over hash conflict. Wake runs only after a
// successful, non-replayed commit.
//
// Result reads re-run the retention verifier every time; a verifier failure is
// reported as unverified retention, not as an error. The Git verifier proves
// the base is a commit reachable from HEAD or an origin tracking ref without
// fetching or mutating refs.
package runapi
