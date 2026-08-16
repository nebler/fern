package proxy

import (
	"fmt"
	"net/http"
)

const landingPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="dark">
<title>Fern</title>
<style>
:root{font-family:ui-rounded,"SF Pro Rounded","Avenir Next",system-ui,sans-serif;color:#f3f7e9;background:#11180f}
*{box-sizing:border-box}body{margin:0;min-height:100dvh;display:grid;place-items:center;padding:max(24px,env(safe-area-inset-top)) 22px max(24px,env(safe-area-inset-bottom));background:radial-gradient(circle at 15% 0,#314829 0,transparent 42%),#11180f}
main{width:min(100%,430px);padding:34px 28px 28px;border:1px solid #52664a;border-radius:28px;background:#182116e8;box-shadow:0 24px 80px #0008}
.mark{display:grid;place-items:center;width:54px;height:54px;border-radius:18px;background:#b9ef86;color:#162210;font-size:28px;font-weight:800;transform:rotate(-3deg)}
h1{margin:24px 0 8px;font-size:36px;letter-spacing:-.04em}p{margin:0;color:#bdcbb5;font-size:17px;line-height:1.55}
.status{display:flex;align-items:center;gap:9px;margin:24px 0;color:#dcebd2;font-size:14px}.dot{width:9px;height:9px;border-radius:50%;background:#b9ef86;box-shadow:0 0 18px #b9ef86}
a{display:block;margin-top:24px;padding:16px 18px;border-radius:16px;background:#b9ef86;color:#15200f;text-align:center;text-decoration:none;font-size:17px;font-weight:750}
small{display:block;margin-top:18px;color:#82917c;line-height:1.45}
</style>
</head>
<body><main><div class="mark">F</div><h1>Your workspace is ready.</h1><p>OpenCode runs behind Fern and wakes automatically when you return.</p><div class="status"><span class="dot"></span> Private gateway connected</div><a href="/">Open OpenCode</a><small>You can close this page at any time. Your sessions and configuration persist while compute sleeps.</small></main></body>
</html>`

func gatewayHandler(upstream http.Handler) http.Handler {
	fern := http.HandlerFunc(serveFern)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/fern" || len(request.URL.Path) > len("/fern/") && request.URL.Path[:len("/fern/")] == "/fern/" || request.URL.Path == "/fern/" {
			fern.ServeHTTP(writer, request)
			return
		}
		upstream.ServeHTTP(writer, request)
	})
}

func serveFern(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	setFernHeaders(writer.Header())
	switch {
	case request.URL.Path == "/fern":
		http.Redirect(writer, request, "/fern/", http.StatusPermanentRedirect)
	case request.URL.Path == "/fern/" && request.URL.EscapedPath() == "/fern/":
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if request.Method == http.MethodGet {
			_, _ = fmt.Fprint(writer, landingPage)
		}
	case request.URL.Path == "/fern/ready" && request.URL.EscapedPath() == "/fern/ready":
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_, _ = fmt.Fprint(writer, `{"ready":true}`)
		}
	default:
		http.NotFound(writer, request)
	}
}

func setFernHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}
