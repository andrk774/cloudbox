package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 — минимальный клиент S3 (SigV4) без внешних зависимостей.
// Проверен с Garage; подойдёт для SeaweedFS, Yandex Object Storage, Selectel и т.п.
type S3 struct {
	endpoint  *url.URL
	region    string
	bucket    string
	access    string
	secret    string
	pathStyle bool
	http      *http.Client
}

func NewS3(endpoint, region, bucket, access, secret string, pathStyle bool) (*S3, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("CB_S3_ENDPOINT: некорректный адрес %q", endpoint)
	}
	if bucket == "" || access == "" || secret == "" {
		return nil, errors.New("для S3 нужны CB_S3_BUCKET, CB_S3_ACCESS_KEY, CB_S3_SECRET_KEY")
	}
	return &S3{endpoint: u, region: region, bucket: bucket, access: access, secret: secret,
		pathStyle: pathStyle, http: &http.Client{Timeout: 0}}, nil
}

// Check проверяет доступ к бакету.
func (s *S3) Check(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodHead, "", nil, -1, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("S3: бакет %q недоступен (HTTP %d)", s.bucket, resp.StatusCode)
	}
	return nil
}

func (s *S3) objectURL(key string) *url.URL {
	u := *s.endpoint
	if s.pathStyle {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + s.bucket
	} else {
		u.Host = s.bucket + "." + u.Host
	}
	if key != "" {
		u.Path += "/" + key
	}
	return &u
}

func (s *S3) do(ctx context.Context, method, key string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	s.sign(req, time.Now().UTC())
	return s.http.Do(req)
}

func (s *S3) sign(req *http.Request, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	req.Header.Set("Host", req.URL.Host)

	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("Range") != "" {
		signed = append(signed, "range")
	}
	sort.Strings(signed) // SigV4 требует алфавитного порядка заголовков
	var canonHdr strings.Builder
	for _, h := range signed {
		v := req.Header.Get(h)
		if h == "host" {
			v = req.URL.Host
		}
		canonHdr.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	canonReq := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonHdr.String(),
		strings.Join(signed, ";"),
		"UNSIGNED-PAYLOAD",
	}, "\n")
	scope := day + "/" + s.region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(canonReq))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])

	k := hmacSHA([]byte("AWS4"+s.secret), day)
	k = hmacSHA(k, s.region)
	k = hmacSHA(k, "s3")
	k = hmacSHA(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.access, scope, strings.Join(signed, ";"), sig))
}

func hmacSHA(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	resp, err := s.do(ctx, http.MethodPut, key, r, size, http.Header{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("S3 PUT %s: HTTP %d %s", key, resp.StatusCode, b)
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, -1, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != 404 {
		return fmt.Errorf("S3 DELETE %s: HTTP %d", key, resp.StatusCode)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (Blob, error) {
	resp, err := s.do(ctx, http.MethodHead, key, nil, -1, nil)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("S3 HEAD %s: HTTP %d", key, resp.StatusCode)
	}
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return &s3Blob{s: s, ctx: ctx, key: key, size: size}, nil
}

// s3Blob читает объект ленивыми Range-запросами, поэтому поддерживает Seek.
type s3Blob struct {
	s    *S3
	ctx  context.Context
	key  string
	size int64
	pos  int64
	body io.ReadCloser
}

func (b *s3Blob) Size() int64 { return b.size }

func (b *s3Blob) Read(p []byte) (int, error) {
	if b.pos >= b.size {
		return 0, io.EOF
	}
	if b.body == nil {
		hdr := http.Header{"Range": {fmt.Sprintf("bytes=%d-", b.pos)}}
		resp, err := b.s.do(b.ctx, http.MethodGet, b.key, nil, -1, hdr)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			resp.Body.Close()
			return 0, fmt.Errorf("S3 GET %s: HTTP %d", b.key, resp.StatusCode)
		}
		b.body = resp.Body
	}
	n, err := b.body.Read(p)
	b.pos += int64(n)
	if err == io.EOF && b.pos < b.size {
		b.body.Close()
		b.body = nil
		err = nil
	}
	return n, err
}

func (b *s3Blob) Seek(off int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = off
	case io.SeekCurrent:
		np = b.pos + off
	case io.SeekEnd:
		np = b.size + off
	}
	if np < 0 {
		return 0, errors.New("negative position")
	}
	if np != b.pos && b.body != nil {
		b.body.Close()
		b.body = nil
	}
	b.pos = np
	return np, nil
}

func (b *s3Blob) Close() error {
	if b.body != nil {
		return b.body.Close()
	}
	return nil
}
