package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/nebler/fern/internal/mcprelay"
)

// t3MCPPath is the one path of T3's server its Claude calls: its MCP server.
const t3MCPPath = "/mcp"

// t3Relay answers for T3's MCP server in an agent box, at the address T3
// names to Claude, and passes each call to daemon, the daemon's route for
// this environment (FERN_T3_MCP), with the box's token; the daemon passes it
// to T3. T3's own token for the thread, in Authorization, goes through as
// Claude sent it. Any other path is refused: only the MCP server is reachable.
func t3Relay(daemon, token string) (http.Handler, error) {
	target, err := url.Parse(daemon)
	if err != nil {
		return nil, err
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host, r.Out.URL.Path, r.Out.URL.RawPath = target.Scheme, target.Host, target.Path, ""
			r.Out.Host = target.Host
			r.Out.Header.Set(mcprelay.BoxTokenHeader, token)
		},
		FlushInterval: -1, // answers may be streams
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != t3MCPPath {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}), nil
}
