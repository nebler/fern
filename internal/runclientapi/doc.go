// Package runclientapi exposes terminal-client run discovery and attachment at
// /fern/api/v1/runs. It is separate from plugin-only runapi submission so that
// trusted operator/device clients get workspace-wide discovery and short-lived
// attachment credentials without being able to create, stop, or seal runs.
//
// Only GET list and GET attach exist. Ingress must already have authenticated
// the request and installed the actor; this package only checks scopes.
// Operator/device actors are trusted by ingress, plugin actors need matching
// credential and scope, and the store keeps plugin ownership hiding.
//
// The list's attachable flag is advisory, not a reservation. Attachment
// requires durable readiness and then a successful issuance from the
// backgroundroute manager, which owns credential expiry and runtime fencing;
// durable state alone never mints access. The returned credentials are secrets:
// responses are no-store and this package never retains or logs them. Nothing
// here proxies the session or changes durable run state.
package runclientapi
