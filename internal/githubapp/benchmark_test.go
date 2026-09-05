package githubapp

import "testing"

// This exercises the production duplicate/depth checks and JSON decoder used
// by discovery, without HTTP, token minting, or RSA key setup.
func BenchmarkDecodeGitHubJSON(b *testing.B) {
	payload := []byte(`{"id":123,"account":{"login":"owner","id":456,"type":"Organization"},"target_type":"Organization","repository_selection":"selected","permissions":{"contents":"write","pull_requests":"write","metadata":"read"},"suspended_at":null}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var response installationAPIResponse
		if err := decodeGitHubJSON(payload, &response); err != nil {
			b.Fatal(err)
		}
	}
}
