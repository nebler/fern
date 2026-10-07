// Package githubapp provides host-side GitHub App onboarding, credential storage,
// repository discovery, and short-lived installation-token minting. App private
// keys remain on the host; the Background Run provider delivers repository-scoped
// installation tokens to containers. This package is not a general GitHub proxy
// or a host-side pull request publisher.
//
// Client.InstallationToken mints a fresh token on every call for exactly one
// repository with contents and pull_requests write, and validates the returned
// permissions and lifetime; caching and delivery belong to taskenvdocker.
// Installation-wide discovery tokens are a separate type used only for operator
// repository selection and are not interchangeable with execution tokens.
// Repository identities and observations are bindings at a point in time, not
// proof of continued access.
//
// The onboarding callback exchanges a one-time manifest code at most once:
// durable claims fence restarts and replays, credentials are saved before the
// claim completes, and ambiguous outcomes are quarantined rather than retried.
// Onboarding states live in the onboarding_states table of Fern's SQLite
// database; each operation is one SQLite transaction, and only digests of the
// state, manifest code, and claim ID are stored.
// CredentialStore provides private-file protection, not encryption at rest;
// encrypted export is credentialbundle's job. Clients disable redirects, and
// public errors never include response bodies or secrets.
package githubapp
