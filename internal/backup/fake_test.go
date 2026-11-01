package backup

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is an in-memory bucket "b" that answers as S3 does to what S3
// uses: multipart uploads, gets, lists and deletes. It checks that every
// request is signed with access key "ak".
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	next    int
}

func newFakeS3(t *testing.T) (*fakeS3, S3) {
	t.Helper()
	f := &fakeS3{objects: make(map[string][]byte), uploads: make(map[string]map[int][]byte)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, S3{Endpoint: srv.URL, Region: "test", Bucket: "b", AccessKey: "ak", SecretKey: "sk", HC: srv.Client()}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/") {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/b/")
	if !ok && r.URL.Path != "/b" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.next++
		id := "u" + strconv.Itoa(f.next)
		f.uploads[id] = make(map[int][]byte)
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts := f.uploads[q.Get("uploadId")]
		var all []byte
		for _, n := range slices.Sorted(func(yield func(int) bool) {
			for n := range parts {
				if !yield(n) {
					return
				}
			}
		}) {
			all = append(all, parts[n]...)
		}
		f.objects[key] = all
		delete(f.uploads, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		type content struct{ Key string }
		var res struct {
			XMLName  xml.Name  `xml:"ListBucketResult"`
			Contents []content `xml:"Contents"`
		}
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				res.Contents = append(res.Contents, content{k})
			}
		}
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(w, bytes.NewReader(data))
	default:
		http.Error(w, "unexpected", http.StatusTeapot)
	}
}
