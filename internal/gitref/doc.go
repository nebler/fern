// Package gitref is the single source of truth for validating Git references,
// SHA-1 object IDs, GitHub owner/repository names, canonical GitHub remotes, and
// repository-relative paths. These validators guard security-sensitive
// boundaries such as GitHub API routes, base branch names, and result manifest
// paths, so packages must delegate here instead of keeping private copies whose
// rules can drift apart.
//
// The rules are Fern policy, deliberately more conservative than Git (for
// example the 255-byte ref limit), not an equivalence test against Git itself.
// ValidateGitHubRemote accepts only the exact spelling
// https://github.com/OWNER/REPOSITORY with no .git suffix, and is the single
// remote-identity check for configuration, run admission, and the Docker
// provider. Path checks are lexical: they never touch the filesystem or resolve
// symlinks. Validate* functions return errors; Valid* functions return booleans.
package gitref
