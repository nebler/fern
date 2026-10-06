package proxy

import "html/template"

// baseCSS is the palette, background, and logo mark shared by every Fern HTML
// page. Pages are served with style-src 'unsafe-inline', so no CSP hash has to
// track this text.
const baseCSS = `:root{font-family:ui-rounded,"SF Pro Rounded","Avenir Next",system-ui,sans-serif;color:#f3f7e9;background:#11180f}
*{box-sizing:border-box}body{margin:0;min-height:100dvh;padding:24px 18px;background:radial-gradient(circle at 15% 0,#314829 0,transparent 42%),#11180f}
.mark{display:grid;place-items:center;width:54px;height:54px;border-radius:18px;background:#b9ef86;color:#162210;font-size:28px;font-weight:800;transform:rotate(-3deg)}`

// dialogCSS centers a single card on the page.
const dialogCSS = `body{display:grid}main{width:min(100%,520px);margin:auto;padding:28px;border:1px solid #52664a;border-radius:26px;background:#182116e8;box-shadow:0 24px 80px #0005}`

// newPage wraps a page body in the shared document shell. title, css, and body
// are trusted constants; dynamic values belong in body template actions.
func newPage(name, title, css, body string) *template.Template {
	return template.Must(template.New(name).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="dark">
<title>` + title + `</title>
<style>
` + baseCSS + `
` + css + `
</style>
</head>
<body>` + body + `</body></html>`))
}
