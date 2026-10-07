// Package proxy assembles Fern's remote and loopback operator HTTP control
// surfaces. Despite the name it does not reverse-proxy OpenCode: live run
// attachment belongs to backgroundroute, and the run handler (runapi) is
// injected by cmd/fern rather than imported. NewHandlers builds handlers only;
// listeners, TLS, and Host validation are part of composition and deployment.
//
// routes.go holds one ServeMux route table per listener and is the complete
// access policy: each route names the realms (public, paired device, plugin
// bearer, operator) that may reach it. A request authenticates as the realm of
// the credential it carries, never by path, and a route that does not admit
// that realm answers 404. Unlisted paths are 404 and listed paths with other
// methods are 405, before authentication. Percent-encoded paths are rejected
// on both listeners because ServeMux matches decoded segments.
//
// The device realm is a durable __Host- cookie; its mutations also need a CSRF
// token bound to credential, method, expiry, and exact path. The plugin realm
// is a pluginauth bearer; fixed scopes are checked by the run APIs. The
// operator listener requires Basic credentials for user "fern", answers any
// plugin bearer with 404, and serves probes before authentication. Every
// realm strips incoming credentials and cookies before dispatch and rejects
// cross-origin mutations.
//
// sameOrigin is a browser policy check, not authentication: a missing Origin
// header is accepted when other checks pass. Pairing codes and their rate
// limits live only in memory, so a restart invalidates outstanding codes.
// Device and plugin revocation persist before in-flight requests are
// cancelled, and cancellation is cooperative, not proof that a downstream
// effect was undone.
package proxy
