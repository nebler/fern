package web

import (
	"errors"
	"net/http"
	"slices"
)

// realm is the set of credential classes a route admits. A request
// authenticates as exactly one realm (chosen by the credential it carries,
// never by its path); a route that does not admit that realm answers 404.
type realm uint8

const (
	device realm = 1 << iota
	plugin
	operator

	// public routes take no credential and strip any that is presented.
	public realm = 0
)

// NewHandlers builds the remote and loopback operator listeners. The two route
// tables below are the complete access policy for each listener: anything not
// listed is 404, a listed path with another method is 405, and every listed
// route names the realms that may reach it.
func NewHandlers(controls Controls, origins TrustedOrigins) (Handlers, error) {
	if controls.Store == nil || controls.PluginAuth == nil || controls.Runs == nil ||
		controls.Liveness == nil || controls.Readiness == nil {
		return Handlers{}, errors.New("control, plugin authorization, run, and probe handlers are required")
	}
	remoteOrigin, operatorOrigin, err := parseTrustedOrigins(origins)
	if err != nil {
		return Handlers{}, err
	}
	operatorAuth, err := newOperatorAuth(controls.Store, controls.ControlAuth)
	if err != nil {
		return Handlers{}, err
	}
	pairing := newPairingState(controls.Store)
	plugins := newPluginAuthHTTP(controls.PluginAuth)
	pages := pages{store: controls.Store}
	devices := deviceControls{store: controls.Store}

	remote := newRouter(func(writer http.ResponseWriter, request *http.Request) (realm, *http.Request, func(), bool) {
		authorization := request.Header.Values("Authorization")
		switch {
		case len(authorization) > 1:
			if slices.ContainsFunc(authorization, bearerLike) {
				rejectPluginBearer(writer)
			} else {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)
			}
			return 0, nil, nil, false
		case slices.ContainsFunc(authorization, bearerLike):
			request, release, ok := plugins.authenticate(writer, request)
			return plugin, request, release, ok
		default:
			request, release, ok := pairing.authenticate(writer, request)
			return device, request, release, ok
		}
	})
	remote.handle("POST /fern/api/plugin-auth/start", public, plugins.start)
	remote.handle("POST /fern/api/plugin-auth/poll", public, plugins.poll)
	remote.handle("GET /fern/pair", public, pairing.pair)
	remote.handle("POST /fern/pair", public, pairing.pair)
	remote.handle("GET /fern", device, redirectToRoot)
	remote.handle("GET /fern/{$}", device, pages.landing)
	remote.handle("GET "+csrfTokenPath, device, serveCSRFToken)
	remote.handle("GET "+pluginAuthorizePath, device, plugins.authorizationPage)
	remote.handle("POST /fern/api/plugin-auth/requests/{id}/approve", device, plugins.decide(true))
	remote.handle("POST /fern/api/plugin-auth/requests/{id}/deny", device, plugins.decide(false))
	remote.handle("POST /fern/api/plugin-auth/self/revoke", plugin, plugins.revokeSelf)
	remote.handle("GET /fern/api/runs", plugin, controls.Runs.ServeHTTP)
	remote.handle("POST /fern/api/runs", plugin, controls.Runs.ServeHTTP)
	remote.handle("GET /fern/api/runs/{id}", plugin, controls.Runs.ServeHTTP)
	remote.handle("POST /fern/api/runs/{id}/stop", plugin, controls.Runs.ServeHTTP)
	remote.handle("GET /fern/api/runs/{id}/result", plugin, controls.Runs.ServeHTTP)
	remote.handle("POST /fern/api/runs/{id}/seal", plugin, controls.Runs.ServeHTTP)
	remote.handle("GET /fern/api/runs/{id}/attach", plugin, controls.Runs.ServeHTTP)

	ops := newRouter(operatorAuth.authenticate)
	ops.handle("GET /fern/live", public, controls.Liveness.ServeHTTP)
	ops.handle("GET /fern/ready", public, controls.Readiness.ServeHTTP)
	ops.handle("GET /fern", operator, redirectToRoot)
	ops.handle("GET /fern/{$}", operator, pages.landing)
	ops.handle("GET /fern/control", operator, pages.control)
	ops.handle("POST /fern/pair/new", operator, pairing.issue)
	ops.handle("GET /fern/api/v1/devices", operator, devices.list)
	ops.handle("DELETE /fern/api/v1/devices/{id}", operator, devices.revoke)
	ops.handle("POST /fern/devices/{id}/revoke", operator, devices.revokeFromPage)
	ops.handle("GET /fern/api/plugin-auth/credentials", operator, plugins.credentials)
	ops.handle("DELETE /fern/api/plugin-auth/credentials/{id}", operator, plugins.revokeCredential)
	ops.handle("POST /fern/api/plugin-auth/requests/{id}/approve", operator, plugins.decide(true))
	ops.handle("POST /fern/api/plugin-auth/requests/{id}/deny", operator, plugins.decide(false))
	ops.handle("GET /fern/api/runs", operator, controls.Runs.ServeHTTP)
	ops.handle("GET /fern/api/runs/{id}", operator, controls.Runs.ServeHTTP)
	ops.handle("GET /fern/api/runs/{id}/attach", operator, controls.Runs.ServeHTTP)

	return Handlers{
		Remote:   listener(remote.mux, remoteOrigin),
		Operator: listener(rejectBearer(ops.mux), operatorOrigin),
	}, nil
}

// authenticator resolves the request's single credential into its realm,
// returning the request carrying the actor and a release for any in-flight
// registration. On failure it has already written the rejection.
type authenticator func(http.ResponseWriter, *http.Request) (realm, *http.Request, func(), bool)

type router struct {
	mux          *http.ServeMux
	authenticate authenticator
}

func newRouter(authenticate authenticator) *router {
	return &router{mux: http.NewServeMux(), authenticate: authenticate}
}

// handle registers one route. Authenticated routes reject any realm they do not
// list as not found, cross-origin mutations as forbidden, and device mutations
// without a CSRF token bound to the credential, method, and exact path.
func (router *router) handle(pattern string, realms realm, next http.HandlerFunc) {
	if realms == public {
		router.mux.HandleFunc(pattern, func(writer http.ResponseWriter, request *http.Request) {
			request.Header.Del("Authorization")
			request.Header.Del("Cookie")
			next(writer, request)
		})
		return
	}
	router.mux.HandleFunc(pattern, func(writer http.ResponseWriter, request *http.Request) {
		got, request, release, ok := router.authenticate(writer, request)
		if !ok {
			return
		}
		defer release()
		if realms&got == 0 {
			http.NotFound(writer, request)
			return
		}
		if isMutation(request) && !sameOrigin(request) {
			http.Error(writer, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if got == device && isMutation(request) && !validDeviceCSRF(request) {
			http.Error(writer, "invalid device CSRF token", http.StatusForbidden)
			return
		}
		next(writer, request)
	})
}

// listener applies the policy shared by every route on one listener: the
// trusted origin for same-origin checks, Fern's browser headers, and rejection
// of percent-encoded paths. ServeMux matches on decoded segments, so without
// this an encoded path could reach a route its raw form does not name.
func listener(next http.Handler, origin trustedOrigin) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		setFernHeaders(writer.Header())
		if request.URL.EscapedPath() != request.URL.Path {
			http.NotFound(writer, request)
			return
		}
		next.ServeHTTP(writer, request.WithContext(withTrustedOrigin(request.Context(), origin)))
	})
}

// rejectBearer makes the operator listener refuse plugin bearer authority
// outright rather than allowing it to be interpreted as Basic.
func rejectBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values := request.Header.Values("Authorization")
		if len(values) > 1 || slices.ContainsFunc(values, bearerLike) {
			http.NotFound(writer, request)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func isMutation(request *http.Request) bool {
	return request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions
}

func noRelease() {}
