// Package storagetest has stand-ins for the services storage locations are kept in, for tests.
package storagetest

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// S3 is an S3-compatible bucket service in memory, path-style (/bucket/key): what Kanade asks of
// R2. It checks that each request is signed by its access key and that bodies are what their
// x-amz-content-sha256 says; Fail can make requests fail.
type S3 struct {
	*httptest.Server
	Bucket, AccessKey, SecretKey string

	mu      sync.Mutex
	objects map[string]*S3Object
	uploads map[string]*s3Upload
	next    int
	// Fail answers a request with a status of its own (0: as usual).
	Fail func(r *http.Request) int
	// Requests counts the requests by method and kind ("PUT part", "PUT", "GET list", ...).
	Requests map[string]int
}

type S3Object struct {
	Data []byte
	Meta http.Header
	ETag string
}

type s3Upload struct {
	name      string
	meta      http.Header
	parts     map[int][]byte
	initiated time.Time
}

func NewS3(t *testing.T) *S3 {
	s := &S3{Bucket: "music", AccessKey: "test-key", SecretKey: "test-secret", objects: map[string]*S3Object{},
		uploads: map[string]*s3Upload{}, Requests: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Object is what the bucket holds at name; nil when nothing.
func (s *S3) Object(name string) *S3Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[name]
}

// Names lists the bucket's object names.
func (s *S3) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Put stores an object as another tool would.
func (s *S3) Put(name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := md5.Sum(data)
	s.objects[name] = &S3Object{Data: data, Meta: http.Header{}, ETag: hex.EncodeToString(sum[:])}
}

// Remove deletes an object as another tool would.
func (s *S3) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, name)
}

// Uploads counts the multipart uploads not completed.
func (s *S3) Uploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// AgeUploads makes the multipart uploads look started d ago.
func (s *S3) AgeUploads(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.uploads {
		u.initiated = u.initiated.Add(-d)
	}
}

func s3Fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

func (s *S3) kind(r *http.Request, name string) string {
	q := r.URL.Query()
	switch {
	case name == "" && q.Has("uploads"):
		return r.Method + " uploads"
	case name == "":
		return r.Method + " list"
	case q.Has("partNumber"):
		return r.Method + " part"
	case q.Has("uploads"):
		return r.Method + " create"
	case q.Has("uploadId") && r.Method == http.MethodPost:
		return "POST complete"
	case q.Has("uploadId") && r.Method == http.MethodGet:
		return "GET parts"
	case q.Has("uploadId"):
		return r.Method + " abort"
	case r.Header.Get("X-Amz-Copy-Source") != "":
		return "PUT copy"
	}
	return r.Method
}

func (s *S3) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	bucket, name, _ := strings.Cut(p, "/")
	kind := s.kind(r, name)
	s.mu.Lock()
	s.Requests[kind]++
	s.mu.Unlock()
	if s.Fail != nil {
		if code := s.Fail(r); code != 0 {
			s3Fail(w, code, "InternalError", "made to fail")
			return
		}
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/") {
		s3Fail(w, http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.")
		return
	}
	if !s.signed(r, auth) {
		s3Fail(w, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		return
	}
	if bucket != s.Bucket {
		s3Fail(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	body, _ := io.ReadAll(r.Body)
	if want := r.Header.Get("X-Amz-Content-Sha256"); want != "UNSIGNED-PAYLOAD" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want {
			s3Fail(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	switch kind {
	case "GET list":
		s.list(w, q)
	case "GET uploads":
		var b bytes.Buffer
		b.WriteString(`<ListMultipartUploadsResult>`)
		for id, u := range s.uploads {
			if strings.HasPrefix(u.name, q.Get("prefix")) {
				fmt.Fprintf(&b, `<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>`, xmlText(u.name), id,
					u.initiated.UTC().Format(time.RFC3339))
			}
		}
		b.WriteString(`</ListMultipartUploadsResult>`)
		w.Write(b.Bytes())
	case "HEAD", "GET":
		o := s.objects[name]
		if o == nil {
			s3Fail(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		for k, v := range o.Meta {
			w.Header()[k] = v
		}
		w.Header().Set("ETag", `"`+o.ETag+`"`)
		w.Header().Set("Accept-Ranges", "bytes")
		data := o.Data
		status := http.StatusOK
		if rng := r.Header.Get("Range"); rng != "" && kind == "GET" {
			var start, end int64 = 0, int64(len(data)) - 1
			spec := strings.TrimPrefix(rng, "bytes=")
			a, b, _ := strings.Cut(spec, "-")
			start, _ = strconv.ParseInt(a, 10, 64)
			if b != "" {
				end, _ = strconv.ParseInt(b, 10, 64)
			}
			end = min(end, int64(len(data))-1)
			if start > end {
				s3Fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			data, status = data[start:end+1], http.StatusPartialContent
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(status)
		if kind == "GET" {
			w.Write(data)
		}
	case "PUT":
		meta := http.Header{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") || k == "Content-Type" {
				meta[k] = v
			}
		}
		sum := md5.Sum(body)
		o := &S3Object{Data: body, Meta: meta, ETag: hex.EncodeToString(sum[:])}
		s.objects[name] = o
		w.Header().Set("ETag", `"`+o.ETag+`"`)
	case "PUT copy":
		src, _ := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
		_, srcName, _ := strings.Cut(strings.TrimPrefix(src, "/"), "/")
		o := s.objects[srcName]
		if o == nil {
			s3Fail(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		cp := *o
		s.objects[name] = &cp
		fmt.Fprintf(w, `<CopyObjectResult><ETag>"%s"</ETag></CopyObjectResult>`, o.ETag)
	case "DELETE":
		delete(s.objects, name)
		w.WriteHeader(http.StatusNoContent)
	case "POST create":
		s.next++
		id := fmt.Sprintf("upload-%d", s.next)
		meta := http.Header{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") || k == "Content-Type" {
				meta[k] = v
			}
		}
		s.uploads[id] = &s3Upload{name: name, meta: meta, parts: map[int][]byte{}, initiated: time.Now()}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
			s.Bucket, xmlText(name), id)
	case "PUT part":
		u := s.uploads[q.Get("uploadId")]
		if u == nil || u.name != name {
			s3Fail(w, http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.")
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		u.parts[n] = body
		sum := md5.Sum(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	case "GET parts":
		u := s.uploads[q.Get("uploadId")]
		if u == nil {
			s3Fail(w, http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.")
			return
		}
		var ns []int
		for n := range u.parts {
			ns = append(ns, n)
		}
		sort.Ints(ns)
		var b bytes.Buffer
		b.WriteString(`<ListPartsResult><IsTruncated>false</IsTruncated>`)
		for _, n := range ns {
			sum := md5.Sum(u.parts[n])
			fmt.Fprintf(&b, `<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag><Size>%d</Size></Part>`, n, hex.EncodeToString(sum[:]), len(u.parts[n]))
		}
		b.WriteString(`</ListPartsResult>`)
		w.Write(b.Bytes())
	case "POST complete":
		u := s.uploads[q.Get("uploadId")]
		if u == nil {
			s3Fail(w, http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.")
			return
		}
		var req struct {
			Parts []struct {
				N    int    `xml:"PartNumber"`
				ETag string `xml:"ETag"`
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &req); err != nil {
			s3Fail(w, http.StatusBadRequest, "MalformedXML", err.Error())
			return
		}
		var data []byte
		var sums []byte
		for i, p := range req.Parts {
			part, ok := u.parts[p.N]
			sum := md5.Sum(part)
			if !ok || p.N != i+1 || strings.Trim(p.ETag, `"`) != hex.EncodeToString(sum[:]) {
				w.WriteHeader(http.StatusOK) // as S3 may: a failure in a 200
				fmt.Fprintf(w, `<Error><Code>InvalidPart</Code><Message>part %d</Message></Error>`, p.N)
				return
			}
			data = append(data, part...)
			sums = append(sums, sum[:]...)
		}
		all := md5.Sum(sums)
		etag := fmt.Sprintf("%s-%d", hex.EncodeToString(all[:]), len(req.Parts))
		s.objects[name] = &S3Object{Data: data, Meta: u.meta, ETag: etag}
		delete(s.uploads, q.Get("uploadId"))
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Key>%s</Key><ETag>"%s"</ETag></CompleteMultipartUploadResult>`, xmlText(name), etag)
	case "DELETE abort":
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Fail(w, http.StatusNotImplemented, "NotImplemented", kind)
	}
}

func (s *S3) list(w http.ResponseWriter, q url.Values) {
	prefix, token := q.Get("prefix"), q.Get("continuation-token")
	limit, _ := strconv.Atoi(q.Get("max-keys"))
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	limit = min(limit, 2) // pages of 2 make continuation tokens show up
	var names []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) && k > token {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString(`<ListBucketResult>`)
	truncated := len(names) > limit
	if truncated {
		names = names[:limit]
	}
	for _, k := range names {
		o := s.objects[k]
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size><ETag>"%s"</ETag></Contents>`, xmlText(k), len(o.Data), o.ETag)
	}
	fmt.Fprintf(&b, `<IsTruncated>%v</IsTruncated>`, truncated)
	if truncated {
		fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, xmlText(names[len(names)-1]))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Write(b.Bytes())
}

func xmlText(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// signed checks a request's Signature Version 4 as the service would: made again from the request
// as it came (its path as sent, its query, the headers it says it signed) with the secret key.
func (s *S3) signed(r *http.Request, auth string) bool {
	var cred, headers, sig string
	for _, part := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			headers = v
		case "Signature":
			sig = v
		}
	}
	scope := strings.SplitN(cred, "/", 2)
	stamp := r.Header.Get("X-Amz-Date")
	if len(scope) != 2 || len(stamp) < 8 || sig == "" {
		return false
	}
	fields := strings.Split(scope[1], "/") // day/region/s3/aws4_request
	if len(fields) != 4 || fields[0] != stamp[:8] {
		return false
	}
	var canon strings.Builder
	for _, h := range strings.Split(headers, ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		canon.WriteString(h + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	q := r.URL.Query()
	var keys []string
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var query []string
	for _, k := range keys {
		for _, v := range q[k] {
			query = append(query, awsEscape(k)+"="+awsEscape(v))
		}
	}
	uri, _, _ := strings.Cut(r.RequestURI, "?")
	creq := strings.Join([]string{r.Method, uri, strings.Join(query, "&"), canon.String(), headers, r.Header.Get("X-Amz-Content-Sha256")}, "\n")
	sum := sha256.Sum256([]byte(creq))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope[1] + "\n" + hex.EncodeToString(sum[:])
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	k := mac([]byte("AWS4"+s.SecretKey), fields[0])
	for _, f := range fields[1:] {
		k = mac(k, f)
	}
	return hmac.Equal([]byte(hex.EncodeToString(mac(k, toSign))), []byte(sig))
}

func awsEscape(v string) string {
	var b strings.Builder
	for _, c := range []byte(v) {
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
