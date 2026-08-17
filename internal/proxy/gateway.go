package proxy

import (
	"html/template"
	"net/http"
	"time"

	"github.com/nebler/fern/internal/control"
)

var landingTemplate = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="dark">
<title>Fern</title>
<style>
:root{font-family:ui-rounded,"SF Pro Rounded","Avenir Next",system-ui,sans-serif;color:#f3f7e9;background:#11180f}
*{box-sizing:border-box}body{margin:0;min-height:100dvh;padding:max(24px,env(safe-area-inset-top)) 18px max(24px,env(safe-area-inset-bottom));background:radial-gradient(circle at 15% 0,#314829 0,transparent 42%),#11180f}
main{width:min(100%,620px);margin:auto}.hero,.panel{padding:28px;border:1px solid #52664a;border-radius:26px;background:#182116e8;box-shadow:0 24px 80px #0005}.panel{margin-top:16px;box-shadow:none}
.mark{display:grid;place-items:center;width:54px;height:54px;border-radius:18px;background:#b9ef86;color:#162210;font-size:28px;font-weight:800;transform:rotate(-3deg)}
h1{margin:24px 0 8px;font-size:36px;letter-spacing:-.04em}h2{margin:0 0 14px;font-size:20px}p{margin:0;color:#bdcbb5;font-size:16px;line-height:1.55}
.status{display:flex;align-items:center;gap:9px;margin:22px 0;color:#dcebd2;font-size:14px}.dot{width:9px;height:9px;border-radius:50%;background:#b9ef86;box-shadow:0 0 18px #b9ef86}
a.primary,button{display:block;width:100%;margin-top:18px;padding:15px 18px;border:0;border-radius:15px;background:#b9ef86;color:#15200f;text-align:center;text-decoration:none;font:inherit;font-weight:750;cursor:pointer}
small,.meta{display:block;margin-top:12px;color:#82917c;line-height:1.45;font-size:13px}ul{list-style:none;padding:0;margin:0}li{padding:13px 0;border-top:1px solid #344230}li:first-child{border-top:0}.title{font-weight:700}.badge{display:inline-block;margin-top:6px;padding:4px 8px;border-radius:99px;background:#2d3c28;color:#cce7ba;font-size:12px}
form.grid{display:grid;gap:10px}input{width:100%;padding:13px 14px;border:1px solid #52664a;border-radius:13px;background:#10180e;color:#f3f7e9;font:inherit}button.danger{width:auto;margin:8px 0 0;padding:8px 11px;background:#472a26;color:#ffcbc2;font-size:13px}
</style>
</head>
<body><main>
<section class="hero"><div class="mark">F</div><h1>Your workspace is ready.</h1><p>OpenCode owns the coding session. Fern keeps remote identity and workflow delivery around it.</p><div class="status"><span class="dot"></span> Private gateway connected</div><a class="primary" href="/">Open OpenCode</a><small>Your OpenCode sessions and configuration persist while compute sleeps.</small></section>
{{if .Control}}
<section class="panel"><h2>Tracked work</h2>{{if .Workflows}}<ul>{{range .Workflows}}<li><div class="title">{{.Title}}</div><span class="badge">{{.Status}}</span><span class="meta">OpenCode session {{.SessionID}}</span>{{if and $.PublicationEnabled (ne .Status "published")}}<form method="post" action="/fern/workflows/{{.ID}}/publish"><button type="submit">{{if .PublicationID}}Retry publication{{else}}Publish draft PR{{end}}</button></form>{{end}}</li>{{end}}</ul>{{else}}<p>No OpenCode sessions are tracked by Fern yet.</p>{{end}}
<form class="grid" method="post" action="/fern/workflows"><input name="title" maxlength="200" placeholder="Workflow title" required><input name="sessionId" maxlength="200" placeholder="OpenCode session ID" required><button type="submit">Track OpenCode session</button></form></section>
<section class="panel"><h2>Paired devices</h2>{{if .Devices}}<ul>{{range .Devices}}<li><div class="title">{{.Name}}</div><span class="meta">Last seen {{.LastSeen.Format "2006-01-02 15:04 UTC"}}</span><form method="post" action="/fern/devices/{{.ID}}/revoke"><button class="danger" type="submit">Revoke</button></form></li>{{end}}</ul>{{else}}<p>No durable devices are paired.</p>{{end}}</section>
{{if .Publications}}<section class="panel"><h2>Publications</h2><ul>{{range .Publications}}<li><div class="title">{{.Title}}</div><span class="badge">{{.State}}</span>{{if .PullURL}}<a class="meta" href="{{.PullURL}}">Open draft pull request</a>{{else if .Commit}}<span class="meta">{{.Branch}} at {{.Commit}}</span>{{end}}</li>{{end}}</ul></section>{{end}}
{{end}}
</main></body></html>`))

type landingView struct {
	Control            bool
	PublicationEnabled bool
	Devices            []control.Device
	Workflows          []control.Workflow
	Publications       []control.Publication
}

func gatewayHandler(upstream http.Handler, controls Controls, publicationEnabled bool) http.Handler {
	fern := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		serveFern(writer, request, controls, publicationEnabled)
	})
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/fern" || request.URL.Path == "/fern/" || len(request.URL.Path) > len("/fern/") && request.URL.Path[:len("/fern/")] == "/fern/" {
			fern.ServeHTTP(writer, request)
			return
		}
		upstream.ServeHTTP(writer, request)
	})
}

func serveFern(writer http.ResponseWriter, request *http.Request, controls Controls, publicationEnabled bool) {
	setFernHeaders(writer.Header())
	if serveControlRoute(writer, request, controls, publicationEnabled) {
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch {
	case request.URL.Path == "/fern":
		http.Redirect(writer, request, "/fern/", http.StatusPermanentRedirect)
	case request.URL.Path == "/fern/" && request.URL.EscapedPath() == "/fern/":
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if request.Method == http.MethodHead {
			return
		}
		view := landingView{Control: controls.Store != nil, PublicationEnabled: publicationEnabled && controls.Fencer != nil && controls.Publisher != nil}
		if controls.Store != nil {
			var err error
			view.Devices, err = controls.Store.Devices(time.Now())
			if err != nil {
				http.Error(writer, "control state unavailable", http.StatusServiceUnavailable)
				return
			}
			view.Workflows = controls.Store.Workflows()
			view.Publications = controls.Store.Publications()
		}
		if err := landingTemplate.Execute(writer, view); err != nil {
			http.Error(writer, "render landing page", http.StatusInternalServerError)
		}
	case request.URL.Path == "/fern/ready" && request.URL.EscapedPath() == "/fern/ready":
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(`{"ready":true}`))
		}
	default:
		http.NotFound(writer, request)
	}
}

func setFernHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
}
