// Package githubapp provides host-side GitHub App credential storage,
// repository discovery, and short-lived installation-token minting. The
// operator creates and installs the App by hand; `fern credentials set`
// validates its ID and private key and stores them here. App private
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
// CredentialStore provides private-file protection, not encryption at rest;
// encrypted export is credentialbundle's job. Clients disable redirects, and
// public errors never include response bodies or secrets.
package githubapp
