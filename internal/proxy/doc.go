// Package proxy assembles Fern's remote and loopback operator HTTP control
// surfaces. Despite the name it does not reverse-proxy OpenCode: live run
// attachment belongs to backgroundroute, and the run handlers (runapi,
// runclientapi) and GitHub onboarding are injected by cmd/fern rather than
// imported. NewHandlers builds handlers only; listeners, TLS, and Host
// validation are part of composition and deployment.
//
// The remote surface admits paired browsers via a durable __Host- device cookie
// plus CSRF tokens bound to credential, method, expiry, and exact path, and
// plugin bearers via pluginauth with fixed scopes and route restrictions. The
// loopback operator surface requires Basic credentials for user "fern",
// explicitly rejects plugin bearers, serves probes before authentication, and
// strips incoming credentials and cookies before dispatch. Status and metrics
// are only on the operator surface.
//
// sameOrigin is a browser policy check, not authentication: a missing Origin
// header is accepted when other checks pass, and device mutations still need
// CSRF. Pairing state is a separate auxiliary file owned here, distinct from
// control and pluginauth state. Device revocation persists before in-flight
// requests are cancelled, and cancellation is cooperative, not proof that a
// downstream effect was undone.
package proxy
