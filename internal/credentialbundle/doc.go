// Package credentialbundle implements the bounded, age-encrypted interchange
// format for Fern credential export and import. It handles encryption and
// structural validation only: compatibility with the running host, activation,
// and live GitHub authorization are decided by the cmd/fern credentials
// commands together with githubapp.
//
// Only bundle version 2 is accepted; version 1 fails explicitly and nothing is
// migrated. Binding validation checks shape (bounded atoms, positive IDs, mode
// github-app-broker), not equality with the host configuration.
//
// Plaintext exists only in memory, never in a file, and Encrypt/Decrypt buffer
// the whole payload. WriteFile installs ciphertext by hard-linking a synced
// private temp file to an absent destination, so it never replaces an existing
// file; a late cleanup or directory-sync error may still leave the destination
// installed. Leaf files are opened no-follow and checked for private mode on
// Darwin/Linux; other platforms fail closed. Callers must supply trusted parent
// directories.
//
// Fingerprint hashes the canonical JSON serialization, not the ciphertext; it
// is not a MAC or an access credential. The exported App private key never
// travels to runs, which receive only short-lived installation tokens.
package credentialbundle
