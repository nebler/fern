package opencodeapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestScanMessages(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("cursor") {
		case "":
			writeJSON(t, writer, `{"data":[{"id":"msg_one","time":{"created":1}}],"cursor":{"next":"next"}}`)
		case "next":
			writeJSON(t, writer, `{"data":[{"id":"msg_two","time":{"created":2}}],"cursor":{}}`)
		default:
			t.Errorf("unexpected cursor")
		}
	}))
	defer server.Close()
	messages, err := testClient(t, server).ScanMessages(deadlineContext(t), "ses_one", ScanOptions{PageLimit: 1, MaxPages: 3, MaxEntries: 3})
	if err != nil || len(messages) != 2 || messages[0].ID != "msg_one" || messages[1].ID != "msg_two" {
		t.Fatalf("ScanMessages = %#v, %v", messages, err)
	}
}

func TestScanPaginationAnomalies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		options ScanOptions
		want    error
	}{
		{
			name: "repeated cursor",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(t, writer, `{"data":[],"cursor":{"next":"same"}}`)
			},
			options: ScanOptions{PageLimit: 2, MaxPages: 3, MaxEntries: 3}, want: ErrProtocolConflict,
		},
		{
			name: "incompatible duplicate bytes",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("cursor") == "" {
					writeJSON(t, writer, `{"data":[{"id":"msg_one","time":{"created":1}}],"cursor":{"next":"next"}}`)
					return
				}
				writeJSON(t, writer, `{"data":[{"id":"msg_one","time":{"created":1},"changed":true}],"cursor":{}}`)
			},
			options: ScanOptions{PageLimit: 1, MaxPages: 3, MaxEntries: 3}, want: ErrProtocolConflict,
		},
		{
			name: "descending order",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(t, writer, `{"data":[{"id":"msg_two","time":{"created":2}},{"id":"msg_one","time":{"created":1}}],"cursor":{}}`)
			},
			options: ScanOptions{PageLimit: 2, MaxPages: 1, MaxEntries: 3}, want: ErrProtocolConflict,
		},
		{
			name: "page bound",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(t, writer, `{"data":[],"cursor":{"next":"more"}}`)
			},
			options: ScanOptions{PageLimit: 1, MaxPages: 1, MaxEntries: 3}, want: ErrScanLimit,
		},
		{
			name: "entry bound",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("cursor") == "" {
					writeJSON(t, writer, `{"data":[{"id":"msg_one","time":{"created":1}}],"cursor":{"next":"next"}}`)
					return
				}
				writeJSON(t, writer, `{"data":[{"id":"msg_two","time":{"created":2}}],"cursor":{}}`)
			},
			options: ScanOptions{PageLimit: 1, MaxPages: 3, MaxEntries: 1}, want: ErrScanLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			_, err := testClient(t, server).ScanMessages(deadlineContext(t), "ses_one", test.options)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestScanHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := testClient(t, server).ScanMessages(ctx, "ses_one", ScanOptions{PageLimit: 1, MaxPages: 2, MaxEntries: 2})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}
