package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An S3-compatible bucket (#100): Cloudflare R2 first, any service that speaks S3's API (MinIO,
// Backblaze B2, AWS S3) as well. The bucket stays private: the server signs every request (AWS
// Signature Version 4) and streams what it reads. A file is sent whole (up to 64 MiB) or in parts
// whose upload is kept across restarts (the upload ID and the parts sent, checked against the
// bucket's list of them when resumed). Every request carries the SHA-256 of its body, which the
// service checks; the stored file's size and the SHA-256 kept in its metadata are checked after.
// Its ETag is never taken for a SHA-256 (a multipart upload's is not even an MD5). Deleted files
// move to .kanade-trash/<date>/ in the bucket and go after 30 days.

// S3Config says where a bucket store is.
type S3Config struct {
	Endpoint string `json:"endpoint"` // https://<account ID>.r2.cloudflarestorage.com
	Region   string `json:"region"`   // "auto" for R2
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix,omitempty"` // a folder in the bucket: "kanade", or "" for all of it
}

// S3Secret is a bucket store's keys (an R2 API token's access key ID and secret).
type S3Secret struct {
	AccessKey string `json:"access_key_id"`
	SecretKey string `json:"secret_access_key"`
}

// s3Backoff is how long a request waits before it is tried again the nth time: a bucket that does
// not answer is said to be so within two seconds.
var s3Backoff = func(n int) time.Duration { return 250 * time.Millisecond << (n - 1) }

// Larger files than s3Whole are sent in parts of at least s3Part (all but the last of the same
// size); variables for tests.
var (
	s3Whole int64 = 64 << 20
	s3Part  int64 = 16 << 20
)

const (
	s3MaxParts   = 10000
	s3UsageEvery = 6 * time.Hour
	s3SweepEvery = 24 * time.Hour
	s3StaleParts = 7 * 24 * time.Hour // multipart uploads left this long are given up (R2 does so too)
	emptySHA256  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	metaSHA256   = "X-Amz-Meta-Sha256"
)

type S3 struct {
	name, mode string
	cfg        S3Config
	sec        S3Secret
	client     *http.Client
	base       *url.URL
	now        func() time.Time

	mu      sync.Mutex
	usage   Usage
	usageAt time.Time
	sweptAt time.Time
}

func newS3(name, mode string, cfg S3Config, sec S3Secret, client *http.Client) (*S3, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("the endpoint must be a URL like https://<account ID>.r2.cloudflarestorage.com")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	return &S3{name: name, mode: mode, cfg: cfg, sec: sec, client: client, base: u, now: time.Now}, nil
}

// cleanPrefix is a bucket folder as kept: no slashes at its ends, nothing like "..".
func cleanPrefix(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".kanade") {
			return "", fmt.Errorf("the folder %q is not a plain path in the bucket", p)
		}
	}
	return p, nil
}

// full is a key's object name in the bucket.
func (s *S3) full(key string) string {
	if s.cfg.Prefix == "" {
		return key
	}
	return s.cfg.Prefix + "/" + key
}

// rel is an object name's key, and whether it is below the prefix.
func (s *S3) rel(name string) (string, bool) {
	if s.cfg.Prefix == "" {
		return name, true
	}
	return strings.CutPrefix(name, s.cfg.Prefix+"/")
}

// ---- requests ----

// s3Encode escapes a string as Signature Version 4 does: everything but A-Z a-z 0-9 - _ . ~ (and
// / when slash is true).
func s3Encode(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, s3Encode(k, false)+"="+s3Encode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// signV4 signs req (AWS Signature Version 4) for payloadHash, the hex SHA-256 of its body. The
// headers signed are the host, the x-amz- ones, and those whose change would change the answer.
func signV4(req *http.Request, accessKey, secretKey, region, payloadHash string, now time.Time) {
	stamp := now.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "range" || lk == "content-type" || lk == "content-md5" {
			headers[lk] = strings.Join(v, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.Join(strings.Fields(headers[k]), " ") + "\n")
	}
	signed := strings.Join(names, ";")
	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	creq := strings.Join([]string{req.Method, uri, canonicalQuery(req.URL.Query()), canon.String(), signed, payloadHash}, "\n")
	sum := sha256.Sum256([]byte(creq))
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+secretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

// s3Request is one request: body makes its body (again for a retry), sized size, whose SHA-256 is
// hash ("" for no body).
type s3Request struct {
	method, name string // name: the object's ("" for the bucket)
	query        url.Values
	header       http.Header
	body         func() (io.Reader, error)
	size         int64
	hash         string
}

// do sends r, trying again after a moment when the service is busy or the network failed. A
// response that is no success is made an error (fail).
func (s *S3) do(ctx context.Context, r s3Request) (*http.Response, error) {
	u := *s.base
	escaped := "/" + s3Encode(s.cfg.Bucket, false)
	raw := "/" + s.cfg.Bucket
	if r.name != "" {
		escaped += "/" + s3Encode(r.name, true)
		raw += "/" + r.name
	}
	u.Path = strings.TrimRight(s.base.Path, "/") + raw
	u.RawPath = strings.TrimRight(s.base.EscapedPath(), "/") + escaped
	u.RawQuery = canonicalQuery(r.query)
	hash := r.hash
	if hash == "" {
		hash = emptySHA256
	}
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(s3Backoff(attempt)):
			}
		}
		var body io.Reader = http.NoBody
		if r.body != nil {
			b, err := r.body()
			if err != nil {
				return nil, err
			}
			body = b
		}
		req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
		if err != nil {
			return nil, err
		}
		req.URL = &u
		if r.body != nil {
			req.ContentLength = r.size
		}
		for k, v := range r.header {
			req.Header[k] = v
		}
		signV4(req, s.sec.AccessKey, s.sec.SecretKey, s.cfg.Region, hash, s.now())
		resp, err := s.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = offline("%s: %v", s.base.Host, unwrapNet(err))
			continue
		}
		if resp.StatusCode < 300 || (resp.StatusCode == http.StatusNotModified) {
			return resp, nil
		}
		last = s.fail(resp)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			continue
		}
		return nil, last
	}
	return nil, last
}

func unwrapNet(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// fail makes a failed response an error: the keys refused, a file not there, the service down or
// busy, the bucket full.
func (s *S3) fail(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e s3Error
	xml.Unmarshal(raw, &e)
	msg := strings.TrimSpace(e.Code + " " + e.Message)
	if msg == "" {
		msg = resp.Status
	}
	switch {
	case e.Code == "NoSuchBucket":
		return fmt.Errorf("the bucket %q does not exist (%s)", s.cfg.Bucket, msg)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case e.Code == "RequestTimeTooSkewed":
		return fmt.Errorf("the server's clock is off; set it right (%s)", msg)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		e.Code == "InvalidAccessKeyId" || e.Code == "SignatureDoesNotMatch" || e.Code == "AccessDenied":
		return fmt.Errorf("%w: %s", ErrAuth, msg)
	case e.Code == "QuotaExceeded" || e.Code == "StorageLimitExceeded" || resp.StatusCode == http.StatusInsufficientStorage:
		return fmt.Errorf("%w: %s", ErrNoRoom, msg)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return offline("%s", msg)
	}
	return fmt.Errorf("%s: %s", resp.Status, msg)
}

func (s *S3) head(ctx context.Context, name string) (Object, http.Header, error) {
	resp, err := s.do(ctx, s3Request{method: http.MethodHead, name: name})
	if err != nil {
		return Object{}, nil, err
	}
	resp.Body.Close()
	return Object{Size: resp.ContentLength, Version: strings.Trim(resp.Header.Get("ETag"), `"`)}, resp.Header, nil
}

// ---- reading ----

func (s *S3) Open(ctx context.Context, key string, start, end int64) (io.ReadCloser, error) {
	h := http.Header{}
	if start > 0 || end >= 0 {
		r := fmt.Sprintf("bytes=%d-", start)
		if end >= 0 {
			r += strconv.FormatInt(end, 10)
		}
		h.Set("Range", r)
	}
	resp, err := s.do(ctx, s3Request{method: http.MethodGet, name: s.full(key), header: h})
	if err != nil {
		return nil, err
	}
	// A range sent says so by its status or its Content-Range: rclone's serve s3 sends one with 200.
	if h.Get("Range") == "" || resp.StatusCode == http.StatusPartialContent || resp.Header.Get("Content-Range") != "" {
		return resp.Body, nil
	}
	// The range not honoured, the whole object sent: what was asked for is cut from it (review #235),
	// as on OneDrive.
	if _, err := io.CopyN(io.Discard, resp.Body, start); err != nil {
		resp.Body.Close()
		return nil, offline("%s: %v", s.base.Host, unwrapNet(err))
	}
	if end < 0 {
		return resp.Body, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, end-start+1), resp.Body}, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Object, error) {
	o, _, err := s.head(ctx, s.full(key))
	o.Key, o.Path = key, key
	return o, err
}

type s3List struct {
	Contents []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
		ETag string `xml:"ETag"`
	} `xml:"Contents"`
	IsTruncated bool   `xml:"IsTruncated"`
	Next        string `xml:"NextContinuationToken"`
}

// list calls fn for every object below prefix (a full name, ending in / or "").
func (s *S3) list(ctx context.Context, prefix string, fn func(name string, size int64, etag string) error) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, s3Request{method: http.MethodGet, query: q})
		if err != nil {
			return err
		}
		var page s3List
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return offline("listing the bucket: %v", err)
		}
		for _, c := range page.Contents {
			if err := fn(c.Key, c.Size, strings.Trim(c.ETag, `"`)); err != nil {
				return err
			}
		}
		if !page.IsTruncated || page.Next == "" {
			return nil
		}
		token = page.Next
	}
}

func (s *S3) prefixDir() string {
	if s.cfg.Prefix == "" {
		return ""
	}
	return s.cfg.Prefix + "/"
}

// Walk lists the files below the prefix; Kanade's own (the trash, the mark) are left out, and so
// are what other tools list as folders and the files in systems' folders, as in a folder.
func (s *S3) Walk(ctx context.Context, fn func(Object) error) error {
	return s.list(ctx, s.prefixDir(), func(name string, size int64, etag string) error {
		key, ok := s.rel(name)
		if !ok || key == "" || strings.HasSuffix(key, "/") || key == markFile || skipPath(key) || skipFile(path.Base(key)) {
			return nil
		}
		return fn(Object{Key: key, Path: key, Size: size, Version: etag})
	})
}

// Check reads the bucket (and, for a managed store, its mark: written when it is not there), and
// now and then sums what it holds and clears out the old trash and given-up uploads.
func (s *S3) Check(ctx context.Context) (Usage, error) {
	if s.mode == Managed {
		if _, _, err := s.head(ctx, s.full(markFile)); errors.Is(err, ErrNotFound) {
			mark := []byte(randomID() + "\n")
			sum := sha256.Sum256(mark)
			resp, err := s.do(ctx, s3Request{method: http.MethodPut, name: s.full(markFile), size: int64(len(mark)), hash: hex.EncodeToString(sum[:]),
				body: func() (io.Reader, error) { return bytes.NewReader(mark), nil }, header: http.Header{"Content-Type": {"text/plain"}}})
			if err != nil {
				return Usage{}, err
			}
			resp.Body.Close()
		} else if err != nil {
			return Usage{}, err
		}
	} else {
		q := url.Values{"list-type": {"2"}, "max-keys": {"1"}}
		if p := s.prefixDir(); p != "" {
			q.Set("prefix", p)
		}
		resp, err := s.do(ctx, s3Request{method: http.MethodGet, query: q})
		if err != nil {
			return Usage{}, err
		}
		resp.Body.Close()
	}
	s.mu.Lock()
	due, sweep := s.now().Sub(s.usageAt) >= s3UsageEvery, s.mode == Managed && s.now().Sub(s.sweptAt) >= s3SweepEvery
	u := s.usage
	s.mu.Unlock()
	if due {
		var n Usage
		err := s.list(ctx, s.prefixDir(), func(name string, size int64, _ string) error {
			n.Used += size
			if key, _ := s.rel(name); !strings.HasPrefix(key, trashDir+"/") && key != markFile {
				n.Files++
			}
			return nil
		})
		if err == nil {
			s.mu.Lock()
			s.usage, s.usageAt, u = n, s.now(), n
			s.mu.Unlock()
		}
	}
	if sweep {
		s.sweep(ctx)
	}
	return u, nil
}

// ---- writing ----

// s3Session is a multipart upload kept across restarts.
type s3Session struct {
	Name   string       `json:"name"` // the object's
	Upload string       `json:"upload"`
	Part   int64        `json:"part"` // the parts' size
	Done   []s3SentPart `json:"done"`
	SHA256 string       `json:"sha256"`
	Size   int64        `json:"size"`
}

type s3SentPart struct {
	N    int    `json:"n"`
	ETag string `json:"etag"`
}

// Put keeps f at Dir/Name, or at "Name (2)" and so on when that holds other content; a file there
// with f's content (size and SHA-256) is f's.
func (s *S3) Put(ctx context.Context, f PutFile) (string, error) {
	if s.mode != Managed {
		return "", ErrReadOnly
	}
	if f.Sessions != nil {
		if raw, err := f.Sessions.LoadSession(ctx); err == nil && raw != "" {
			var ses s3Session
			if json.Unmarshal([]byte(raw), &ses) == nil && ses.Upload != "" && ses.SHA256 == f.SHA256 && ses.Size == f.Size {
				key, err := s.resume(ctx, f, &ses)
				if err == nil || !errors.Is(err, errUploadGone) {
					return key, err
				}
			}
			f.Sessions.DeleteSession(ctx)
		}
	}
	dir := strings.Trim(path.Clean("/"+f.Dir), "/")
	ext := path.Ext(f.Name)
	base := strings.TrimSuffix(f.Name, ext)
	for n := 1; n < 100; n++ {
		name := f.Name
		if n > 1 {
			name = fmt.Sprintf("%s (%d)%s", base, n, ext)
		}
		key := name
		if dir != "" {
			key = dir + "/" + name
		}
		o, h, err := s.head(ctx, s.full(key))
		switch {
		case err == nil:
			if o.Size == f.Size && strings.EqualFold(h.Get(metaSHA256), f.SHA256) {
				return key, nil // there already: an earlier attempt, or the same file again
			}
			continue
		case !errors.Is(err, ErrNotFound):
			return "", err
		}
		if f.Size <= s3Whole {
			return key, s.putWhole(ctx, f, key)
		}
		ses := &s3Session{Name: s.full(key), SHA256: f.SHA256, Size: f.Size, Part: max(s3Part, (f.Size+s3MaxParts-1)/s3MaxParts)}
		return s.resume(ctx, f, ses)
	}
	return "", fmt.Errorf("no free name for %s in %s", f.Name, f.Dir)
}

func (s *S3) putWhole(ctx context.Context, f PutFile, key string) error {
	file, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	h := http.Header{metaSHA256: {strings.ToLower(f.SHA256)}}
	if f.MIME != "" {
		h.Set("Content-Type", f.MIME)
	}
	resp, err := s.do(ctx, s3Request{method: http.MethodPut, name: s.full(key), header: h, size: f.Size, hash: strings.ToLower(f.SHA256),
		body: func() (io.Reader, error) {
			return &progressReader{r: io.NewSectionReader(file, 0, f.Size), total: f.Size, fn: f.Progress}, nil
		}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return s.verify(ctx, f, s.full(key))
}

// stale has the next check sum what the bucket holds again.
func (s *S3) stale() {
	s.mu.Lock()
	s.usageAt = time.Time{}
	s.mu.Unlock()
}

// verify checks a stored copy: its size, and the SHA-256 it keeps. One that is not f is removed
// (it was just written under a free name: nothing else is there).
func (s *S3) verify(ctx context.Context, f PutFile, name string) error {
	s.stale()
	o, h, err := s.head(ctx, name)
	if err != nil {
		return err
	}
	if o.Size == f.Size && strings.EqualFold(h.Get(metaSHA256), f.SHA256) {
		return nil
	}
	if resp, err := s.do(ctx, s3Request{method: http.MethodDelete, name: name}); err == nil {
		resp.Body.Close()
	}
	return fmt.Errorf("the stored copy is not the file (%d bytes, SHA-256 %q); it was removed", o.Size, h.Get(metaSHA256))
}

var errUploadGone = errors.New("the multipart upload is no longer there")

type s3Parts struct {
	Parts []struct {
		N    int    `xml:"PartNumber"`
		ETag string `xml:"ETag"`
		Size int64  `xml:"Size"`
	} `xml:"Part"`
	IsTruncated bool `xml:"IsTruncated"`
	Next        int  `xml:"NextPartNumberMarker"`
}

// resume sends what is left of a multipart upload (all of it for a new one) and completes it.
func (s *S3) resume(ctx context.Context, f PutFile, ses *s3Session) (string, error) {
	key, ok := s.rel(ses.Name)
	if !ok {
		return "", errUploadGone
	}
	save := func() error {
		if f.Sessions == nil {
			return nil
		}
		raw, _ := json.Marshal(ses)
		return f.Sessions.SaveSession(ctx, string(raw))
	}
	parts := int((f.Size + ses.Part - 1) / ses.Part)
	sizeOf := func(n int) int64 { return min(ses.Part, f.Size-int64(n-1)*ses.Part) }
	done := map[int]string{}
	if ses.Upload == "" {
		h := http.Header{metaSHA256: {strings.ToLower(f.SHA256)}}
		if f.MIME != "" {
			h.Set("Content-Type", f.MIME)
		}
		resp, err := s.do(ctx, s3Request{method: http.MethodPost, name: ses.Name, query: url.Values{"uploads": {""}}, header: h})
		if err != nil {
			return "", err
		}
		var v struct {
			UploadID string `xml:"UploadId"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if err != nil || v.UploadID == "" {
			return "", fmt.Errorf("starting a multipart upload: %v", err)
		}
		ses.Upload, ses.Done = v.UploadID, nil
		if err := save(); err != nil {
			return "", err
		}
	} else {
		// What was sent is what the bucket lists as sent, by the same ETag: a part sent once the
		// session was last saved is sent again.
		recorded := map[int]string{}
		for _, p := range ses.Done {
			recorded[p.N] = p.ETag
		}
		marker := 0
		for {
			q := url.Values{"uploadId": {ses.Upload}, "max-parts": {"1000"}}
			if marker > 0 {
				q.Set("part-number-marker", strconv.Itoa(marker))
			}
			resp, err := s.do(ctx, s3Request{method: http.MethodGet, name: ses.Name, query: q})
			if errors.Is(err, ErrNotFound) {
				return "", errUploadGone
			}
			if err != nil {
				return "", err
			}
			var page s3Parts
			err = xml.NewDecoder(resp.Body).Decode(&page)
			resp.Body.Close()
			if err != nil {
				return "", offline("listing the parts sent: %v", err)
			}
			for _, p := range page.Parts {
				etag := strings.Trim(p.ETag, `"`)
				if p.N >= 1 && p.N <= parts && p.Size == sizeOf(p.N) && recorded[p.N] == etag {
					done[p.N] = etag
				}
			}
			if !page.IsTruncated || page.Next <= marker {
				break
			}
			marker = page.Next
		}
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var sent int64
	for n := 1; n <= parts; n++ {
		if _, ok := done[n]; ok {
			sent += sizeOf(n)
		}
	}
	for n := 1; n <= parts; n++ {
		if _, ok := done[n]; ok {
			continue
		}
		off, size := int64(n-1)*ses.Part, sizeOf(n)
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(file, off, size)); err != nil {
			return "", err
		}
		base := sent
		resp, err := s.do(ctx, s3Request{method: http.MethodPut, name: ses.Name, size: size, hash: hex.EncodeToString(h.Sum(nil)),
			query: url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {ses.Upload}},
			body: func() (io.Reader, error) {
				return &progressReader{r: io.NewSectionReader(file, off, size), base: base, total: f.Size, fn: f.Progress}, nil
			}})
		if errors.Is(err, ErrNotFound) {
			return "", errUploadGone
		}
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		if etag == "" {
			return "", errors.New("the bucket gave no ETag for a part")
		}
		done[n] = etag
		ses.Done = append(ses.Done, s3SentPart{N: n, ETag: etag})
		sent += size
		if err := save(); err != nil {
			return "", err
		}
	}
	var body bytes.Buffer
	body.WriteString("<CompleteMultipartUpload>")
	for n := 1; n <= parts; n++ {
		fmt.Fprintf(&body, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, n, done[n])
	}
	body.WriteString("</CompleteMultipartUpload>")
	payload := body.Bytes()
	sum := sha256.Sum256(payload)
	resp, err := s.do(ctx, s3Request{method: http.MethodPost, name: ses.Name, query: url.Values{"uploadId": {ses.Upload}},
		size: int64(len(payload)), hash: hex.EncodeToString(sum[:]), header: http.Header{"Content-Type": {"application/xml"}},
		body: func() (io.Reader, error) { return bytes.NewReader(payload), nil }})
	if errors.Is(err, ErrNotFound) {
		return "", errUploadGone
	}
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var e s3Error
	if xml.Unmarshal(raw, &e) == nil && e.Code != "" { // S3 may say it failed in a 200
		return "", fmt.Errorf("completing the upload: %s %s", e.Code, e.Message)
	}
	if err := s.verify(ctx, f, ses.Name); err != nil {
		return "", err
	}
	if f.Sessions != nil {
		f.Sessions.DeleteSession(ctx)
	}
	return key, nil
}

type progressReader struct {
	r           io.Reader
	base, total int64
	read        int64
	fn          func(sent, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.fn != nil && n > 0 {
		p.fn(p.base+p.read, p.total)
	}
	return n, err
}

// Trash moves key to .kanade-trash/<date>/ in the bucket (a copy, then the delete).
func (s *S3) Trash(ctx context.Context, key string) error {
	if s.mode != Managed {
		return ErrReadOnly
	}
	src := s.full(key)
	dst := s.full(trashDir + "/" + s.now().UTC().Format("20060102") + "/" + key)
	resp, err := s.do(ctx, s3Request{method: http.MethodPut, name: dst,
		header: http.Header{"X-Amz-Copy-Source": {"/" + s3Encode(s.cfg.Bucket, false) + "/" + s3Encode(src, true)}}})
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var e s3Error
	if xml.Unmarshal(raw, &e) == nil && e.Code != "" {
		return fmt.Errorf("copying to the trash: %s %s", e.Code, e.Message)
	}
	resp, err = s.do(ctx, s3Request{method: http.MethodDelete, name: src})
	if err != nil {
		return err
	}
	resp.Body.Close()
	s.stale()
	return nil
}

// sweep deletes the trash older than 30 days and multipart uploads left for a week.
func (s *S3) sweep(ctx context.Context) {
	cutoff := s.now().UTC().AddDate(0, 0, -trashDays).Format("20060102")
	var old []string
	s.list(ctx, s.full(trashDir+"/"), func(name string, _ int64, _ string) error {
		key, _ := s.rel(name)
		day, _, _ := strings.Cut(strings.TrimPrefix(key, trashDir+"/"), "/")
		if len(day) == 8 && day < cutoff {
			old = append(old, name)
		}
		return nil
	})
	for _, name := range old {
		if resp, err := s.do(ctx, s3Request{method: http.MethodDelete, name: name}); err == nil {
			resp.Body.Close()
		}
	}
	q := url.Values{"uploads": {""}}
	if p := s.prefixDir(); p != "" {
		q.Set("prefix", p)
	}
	if resp, err := s.do(ctx, s3Request{method: http.MethodGet, query: q}); err == nil {
		var v struct {
			Uploads []struct {
				Key       string    `xml:"Key"`
				UploadID  string    `xml:"UploadId"`
				Initiated time.Time `xml:"Initiated"`
			} `xml:"Upload"`
		}
		xml.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		for _, u := range v.Uploads {
			if !u.Initiated.IsZero() && s.now().Sub(u.Initiated) > s3StaleParts {
				if r, err := s.do(ctx, s3Request{method: http.MethodDelete, name: u.Key, query: url.Values{"uploadId": {u.UploadID}}}); err == nil {
					r.Body.Close()
				}
			}
		}
	}
	s.mu.Lock()
	s.sweptAt = s.now()
	s.mu.Unlock()
}
