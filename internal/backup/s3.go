package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// S3 is a bucket of an S3-compatible object store (Hetzner's, R2, MinIO),
// with only what backups need: objects put in parts, read, listed and
// deleted. Requests are signed with AWS Signature Version 4.
type S3 struct {
	Endpoint  string // https://fsn1.your-objectstorage.com
	Region    string // fsn1
	Bucket    string
	AccessKey string
	SecretKey string
	HC        *http.Client
}

// ErrNoObject means the object doesn't exist.
var ErrNoObject = errors.New("no such object")

// partSize is how much of a stream one part of an upload holds: S3 wants
// at least 5 MiB for every part but the last.
const partSize = 16 << 20

// Put stores what r yields as object key, in parts, so that r's length
// needn't be known; an upload that fails is aborted.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	if err := s.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, &created); err != nil {
		return 0, fmt.Errorf("start upload of %s: %w", key, err)
	}
	type part struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var parts []part
	var total int64
	buf := make([]byte, partSize)
	for n := 1; ; n++ {
		size, err := io.ReadFull(r, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			s.abort(ctx, key, created.UploadID)
			return total, err
		}
		if size == 0 && n > 1 {
			break
		}
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {created.UploadID}}
		resp, perr := s.send(ctx, http.MethodPut, key, q, buf[:size])
		if perr != nil {
			s.abort(ctx, key, created.UploadID)
			return total, fmt.Errorf("upload part %d of %s: %w", n, key, perr)
		}
		_ = resp.Body.Close()
		parts = append(parts, part{Number: n, ETag: resp.Header.Get("ETag")})
		total += int64(size)
		if size < partSize {
			break
		}
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{Parts: parts})
	if err != nil {
		return total, err
	}
	if err := s.do(ctx, http.MethodPost, key, url.Values{"uploadId": {created.UploadID}}, body, nil); err != nil {
		s.abort(ctx, key, created.UploadID)
		return total, fmt.Errorf("finish upload of %s: %w", key, err)
	}
	return total, nil
}

func (s *S3) abort(ctx context.Context, key, upload string) {
	_ = s.do(context.WithoutCancel(ctx), http.MethodDelete, key, url.Values{"uploadId": {upload}}, nil, nil)
}

// Get returns object key's content; the caller closes it.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.send(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// List returns the keys under prefix.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
		}
		if err := s.do(ctx, http.MethodGet, "", q, nil, &page); err != nil {
			return nil, err
		}
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
		}
		if !page.Truncated {
			return keys, nil
		}
		token = page.Next
	}
}

// Delete removes object key; one that doesn't exist is no error.
func (s *S3) Delete(ctx context.Context, key string) error {
	err := s.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if errors.Is(err, ErrNoObject) {
		return nil
	}
	return err
}

// do sends a request and decodes its XML answer into out, if out isn't nil.
func (s *S3) do(ctx context.Context, method, key string, q url.Values, body []byte, out any) error {
	resp, err := s.send(ctx, method, key, q, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	return xml.NewDecoder(resp.Body).Decode(out)
}

// send signs and sends a request to the bucket, path style, and returns a
// successful response; the caller closes its body.
func (s *S3) send(ctx context.Context, method, key string, q url.Values, body []byte) (*http.Response, error) {
	u, err := url.Parse(strings.TrimSuffix(s.Endpoint, "/") + "/" + s.Bucket + "/" + key)
	if err != nil {
		return nil, err
	}
	u.RawQuery = canonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	s.sign(req, body, time.Now().UTC())
	resp, err := s.HC.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s %s: %w", method, key, ErrNoObject)
	}
	return nil, fmt.Errorf("%s %s: %s: %s", method, key, resp.Status, bytes.TrimSpace(msg))
}

// sign adds AWS Signature Version 4 headers to req, whose body is body, as
// of now.
func (s *S3) sign(req *http.Request, body []byte, now time.Time) {
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	date := now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", date)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Host", req.URL.Host)
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	var headers strings.Builder
	for _, n := range names {
		v := req.URL.Host
		if n != "host" {
			v = req.Header.Get(n)
		}
		fmt.Fprintf(&headers, "%s:%s\n", n, strings.TrimSpace(v))
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{
		req.Method, req.URL.EscapedPath(), req.URL.RawQuery, headers.String(), signed, payload,
	}, "\n")
	scope := date[:8] + "/" + s.Region + "/s3/aws4_request"
	chash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(chash[:])
	key := []byte("AWS4" + s.SecretKey)
	for _, part := range []string{date[:8], s.Region, "s3", "aws4_request"} {
		key = mac(key, part)
	}
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.AccessKey, scope, signed, hex.EncodeToString(mac(key, toSign))))
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQuery encodes q as SigV4 wants it: sorted, with %20 for spaces.
func canonicalQuery(q url.Values) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(q)) {
		for _, v := range q[k] {
			parts = append(parts, escape(k)+"="+escape(v))
		}
	}
	return strings.Join(parts, "&")
}

func escape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
