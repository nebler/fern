// Package config loads and validates Fern's single current run
// configuration shape. One strict YAML document (at most MaxConfigBytes) is the
// single source of truth; there are no CLI overrides. Obsolete or unknown
// fields are rejected rather than translated; there is no legacy execution
// shape.
//
// Loading does not validate: callers choose ValidateWorkspace (offline shape and
// directory check), ValidateBootstrap (permits a pending App installation), or
// Validate (requires an installed execution binding). None of them contact
// GitHub, Docker, Git, or DNS, so a valid Config is not evidence that the
// repository, image, installation, or runtime storage quota actually exist;
// those are established by the components that use them.
//
// Nothing in the file undergoes environment expansion; the control password is
// never in the file and comes from FERN_CONTROL_PASSWORD. A relative
// workspace.repo resolves against the config file's directory. The configuration
// binds GitHub installation and repository IDs but never holds the App private
// key.
package config
