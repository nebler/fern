package hibernate

import (
	"bytes"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// lines splits data after each newline; the last line may have none.
func lines(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return append(out, data)
		}
		out = append(out, data[:i+1])
		data = data[i+1:]
	}
	return out
}

// The SDK's lines reach Claude as they came, each whole and with its
// newline, whatever they hold.
func FuzzReadLines(f *testing.F) {
	f.Add([]byte(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" + `{"type":"control_request","request_id":"1","request":{"subtype":"initialize"}}` + "\n"))
	f.Add([]byte("no newline at the end"))
	f.Add([]byte("\n\n\x00{\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var got [][]byte
		for line := range readLines(make(chan struct{}), bytes.NewReader(data)) {
			got = append(got, line)
		}
		if diff := cmp.Diff(lines(data), got); diff != "" {
			t.Errorf("readLines() (-want +got):\n%s", diff)
		}
	})
}

// What Claude prints reaches the SDK in whole lines it printed, in order,
// however its writes cut them: none is made up, cut or joined, and the only
// ones kept back are answers to the setup a restarted Claude was sent again.
// The SDK's side of the stream (In) is fed first from the same lines, so
// that a restart has requests to replay.
func FuzzLineWriter(f *testing.F) {
	setup := `{"type":"control_request","request_id":"r1","request":{"subtype":"initialize"}}` + "\n"
	f.Add([]byte(setup+`{"type":"control_response","response":{"request_id":"r1","subtype":"success"}}`+"\n"+`{"type":"result"}`+"\n"), uint8(7))
	f.Add([]byte(`{"type":"system","subtype":"init","session_id":"s"}`+"\npartial"), uint8(1))
	f.Add([]byte(setup+setup+`{"type":"control_response","response":{"request_id":"r1"}}`+"\n"+`{"type":"control_response","response":{"request_id":"r1"}}`), uint8(0))
	f.Add([]byte("\n\n{\"type\":\"control_response\",\"response\":null}\n"), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		st := newStream()
		in := lines(data)
		for _, line := range in {
			st.In(line)
		}
		st.restart([]string{"claude"})
		var out bytes.Buffer
		type seen struct {
			line string
			kept bool
		}
		var asked []seen
		w := &lineWriter{w: &out, keep: func(line []byte) bool {
			ok := st.Out(line)
			asked = append(asked, seen{string(line), ok})
			return ok
		}}
		size := int(chunk) + 1
		for rest := data; len(rest) > 0; {
			n := min(size, len(rest))
			if got, err := w.Write(rest[:n]); got != n || err != nil {
				t.Fatalf("Write() = %d, %v; want %d", got, err, n)
			}
			rest = rest[n:]
		}
		if err := w.flush(); err != nil {
			t.Fatal(err)
		}
		// keep is asked of each line Claude printed, whole and in order, and
		// what it allows is passed on with its newline as Claude printed it.
		var want bytes.Buffer
		var wantAsked []seen
		for i, line := range in {
			body := bytes.TrimSuffix(line, []byte("\n"))
			if i < len(asked) && asked[i].kept {
				want.Write(line)
			} else if !bytes.Contains(body, []byte("control_response")) {
				t.Errorf("line %q was kept back: only answers to the setup may be", body)
			}
			wantAsked = append(wantAsked, seen{string(body), i < len(asked) && asked[i].kept})
		}
		if diff := cmp.Diff(wantAsked, asked, cmp.AllowUnexported(seen{})); diff != "" {
			t.Errorf("the lines keep was asked of (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(want.String(), out.String()); diff != "" {
			t.Errorf("what the SDK got (-want +got):\n%s", diff)
		}
	})
}
