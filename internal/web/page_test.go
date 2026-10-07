package web

import (
	"html/template"
	"strings"
	"testing"
)

func TestPagesShareShell(t *testing.T) {
	for _, page := range []struct {
		template *template.Template
		data     any
		want     []string
	}{
		{landingTemplate, landingView{}, []string{"<title>Fern</title>", "Fern Background Runs"}},
		{pairingTemplate, pairingPage{Code: "c<de", Name: "phone"}, []string{"<title>Pair with Fern</title>", `value="c&lt;de"`}},
		{deviceRevokedTemplate, nil, []string{"<title>Device revoked</title>", "<h1>Device revoked</h1>"}},
		{pluginAuthorizationTemplate, pluginAuthorizationPage{Client: pluginClientName, Code: "ABCD", Nonce: "n0nce"}, []string{"<title>Authorize OpenCode</title>", `<script nonce="n0nce">`}},
	} {
		var out strings.Builder
		if err := page.template.Execute(&out, page.data); err != nil {
			t.Fatalf("%s: %v", page.template.Name(), err)
		}
		html := out.String()
		for _, want := range append(page.want, "<!doctype html>", baseCSS, "</body></html>") {
			if !strings.Contains(html, want) {
				t.Errorf("%s missing %q", page.template.Name(), want)
			}
		}
	}
}
