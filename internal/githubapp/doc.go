// Package githubapp provides host-side GitHub App onboarding, credential storage,
// repository discovery, and short-lived installation-token minting. App private
// keys remain on the host; the Background Run provider delivers repository-scoped
// installation tokens to containers. This package is not a general GitHub proxy
// or a host-side pull request publisher.
package githubapp
