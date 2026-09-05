package strictjson

import (
	"strings"
	"testing"
)

func BenchmarkCheck(b *testing.B) {
	entry := `{"id":"msg_fixture","role":"assistant","parts":[{"type":"text","text":"retained result fixture"}],"usage":{"input":123,"output":45}}`
	history := `{"messages":[` + strings.TrimSuffix(strings.Repeat(entry+",", 64), ",") + `],"next":null}`
	cases := []struct {
		name    string
		payload []byte
		wantErr bool
	}{
		{name: "RunRequest", payload: []byte(`{"title":"Review change","prompt":"Check the tests","base":{"ref":"main","sha":"0123456789012345678901234567890123456789"}}`)},
		{name: "History64", payload: []byte(history)},
		{name: "LateDuplicateKey", payload: []byte(`{"history":` + history + `,"HISTORY":null}`), wantErr: true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			if !tc.wantErr {
				b.SetBytes(int64(len(tc.payload)))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := Check(tc.payload, 32)
				if (err != nil) != tc.wantErr {
					b.Fatalf("Check error = %v, want error = %t", err, tc.wantErr)
				}
			}
		})
	}
}
