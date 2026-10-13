package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/storage/storagetest"
)

type testSessions struct{ v string }

func (m *testSessions) LoadSession(context.Context) (string, error)   { return m.v, nil }
func (m *testSessions) SaveSession(_ context.Context, v string) error { m.v = v; return nil }
func (m *testSessions) DeleteSession(context.Context) error           { m.v = ""; return nil }
func (m *testSessions) session(t *testing.T) (s s3Session)            { json.Unmarshal([]byte(m.v), &s); return }

var _ Sessions = (*testSessions)(nil)

func addBucket(t *testing.T, s *Set, fake *storagetest.S3, prefix, mode, secret string) (*Store, error) {
	t.Helper()
	cfg, _ := json.Marshal(S3Config{Endpoint: fake.URL, Bucket: fake.Bucket, Prefix: prefix})
	sec, _ := json.Marshal(S3Secret{AccessKey: fake.AccessKey, SecretKey: secret})
	return s.Add(context.Background(), NewStore{Kind: KindS3, Name: "R2 " + mode, Mode: mode, Config: cfg, Secret: sec})
}

// A bucket as a managed store: added (its keys tried by writing its mark), files kept and read
// back in ranges, the same file kept once, another under a free name, large ones in parts that
// resume after a failure and are checked, the trash and its sweep, and what failures are said as.
func TestS3Managed(t *testing.T) {
	ctx := context.Background()
	defer func(w, p int64, b func(int) time.Duration) { s3Whole, s3Part, s3Backoff = w, p, b }(s3Whole, s3Part, s3Backoff)
	s3Whole, s3Part, s3Backoff = 1000, 300, func(int) time.Duration { return time.Millisecond }
	fake := storagetest.NewS3(t)
	s, data := newSet(t)
	if _, err := addBucket(t, s, fake, "kanade", Managed, "wrong"); err == nil {
		t.Fatal("added with keys the bucket refuses") // the fake takes any secret, so try a wrong key ID
	}
	fake.AccessKey = "other"
	var bad *ErrInvalid
	if _, err := addBucket(t, s, fake, "kanade", Managed, "x"); !errors.As(err, &bad) || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("wrong keys: %v", err)
	}
	fake.AccessKey = "test-key"
	st, err := addBucket(t, s, fake, "/kanade/", Managed, fake.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateOK || fake.Object("kanade/.kanade-store") == nil {
		t.Fatalf("added: %+v %v", st, fake.Names())
	}
	if _, err := addBucket(t, s, fake, "kanade/inner", Source, fake.SecretKey); !errors.As(err, &bad) || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("an overlapping folder: %v", err)
	}
	dest, err := s.Destination(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}

	// A small file: whole, checked, read back.
	p, sha := file(t, data, "a.flac", "fLaC-0123456789")
	ref, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/Artist/Album", Name: "01 Song.flac", MIME: "audio/flac", Size: 15, SHA256: sha})
	if err != nil || ref != "s3:1:library/Artist/Album/01 Song.flac" {
		t.Fatalf("put: %q %v", ref, err)
	}
	o := fake.Object("kanade/library/Artist/Album/01 Song.flac")
	if o == nil || o.Meta.Get("X-Amz-Meta-Sha256") != sha || o.Meta.Get("Content-Type") != "audio/flac" {
		t.Fatalf("stored: %+v", o)
	}
	if got := read(t, s, ref, 4, 7); got != "-012" {
		t.Fatalf("range: %q", got)
	}
	if got := read(t, s, ref, 5, -1); got != "0123456789" {
		t.Fatalf("to the end: %q", got)
	}
	puts := fake.Requests["PUT"]
	if again, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/Artist/Album", Name: "01 Song.flac", Size: 15, SHA256: sha}); err != nil || again != ref ||
		fake.Requests["PUT"] != puts {
		t.Fatalf("the same file again: %q %v (%d puts)", again, err, fake.Requests["PUT"]-puts)
	}
	p2, sha2 := file(t, data, "b.flac", "fLaC-other")
	if other, err := dest.Put(ctx, PutFile{Path: p2, Dir: "library/Artist/Album", Name: "01 Song.flac", Size: 10, SHA256: sha2}); err != nil ||
		other != "s3:1:library/Artist/Album/01 Song (2).flac" {
		t.Fatalf("other content, same name: %q %v", other, err)
	}

	// A large file: in parts; the third fails for good, and the upload resumes from it.
	big := strings.Repeat("0123456789", 250) // 2500 bytes: parts of 300
	p3, sha3 := file(t, data, "c.flac", big)
	ses := &testSessions{}
	failed := 0
	fake.Fail = func(r *http.Request) int {
		if r.URL.Query().Get("partNumber") == "3" && failed == 0 {
			failed++
			return http.StatusForbidden
		}
		return 0
	}
	f := PutFile{Path: p3, Dir: "library/Artist/Album", Name: "02 Long.flac", Size: int64(len(big)), SHA256: sha3, Sessions: ses}
	if _, err := dest.Put(ctx, f); !errors.Is(err, ErrAuth) {
		t.Fatalf("a part refused: %v", err)
	}
	if got := ses.session(t); got.Upload == "" || len(got.Done) != 2 || got.Part != 300 {
		t.Fatalf("the session kept: %+v", got)
	}
	parts := fake.Requests["PUT part"]
	var sent []int64
	f.Progress = func(n, total int64) { sent = append(sent, n) }
	ref3, err := dest.Put(ctx, f)
	if err != nil || ref3 != "s3:1:library/Artist/Album/02 Long.flac" {
		t.Fatalf("resumed: %q %v", ref3, err)
	}
	if n := fake.Requests["PUT part"] - parts; n != 7 { // parts 3 to 9
		t.Fatalf("resumed with %d parts sent", n)
	}
	if sent[0] <= 600 || sent[len(sent)-1] != int64(len(big)) {
		t.Fatalf("progress from where it was: %v", sent)
	}
	if o := fake.Object("kanade/library/Artist/Album/02 Long.flac"); o == nil || string(o.Data) != big || !strings.HasSuffix(o.ETag, "-9") {
		t.Fatal("the large file is not what was sent")
	}
	if ses.v != "" || fake.Uploads() != 0 {
		t.Fatalf("left: session %q, %d uploads", ses.v, fake.Uploads())
	}
	// An upload the bucket gave up: started again.
	ses.v = `{"name":"kanade/library/Artist/Album/03.flac","upload":"gone","part":300,"done":[{"n":1,"etag":"x"}],"sha256":"` + sha3 + `","size":2500}`
	f.Name = "03.flac"
	if ref, err := dest.Put(ctx, f); err != nil || ref != "s3:1:library/Artist/Album/03.flac" {
		t.Fatalf("after the upload was given up: %q %v", ref, err)
	}

	// Listing, room, the trash.
	var keys []string
	b, _, _ := s.Backend(ctx, 1)
	if err := b.Walk(ctx, func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "library/Artist/Album/01 Song (2).flac,library/Artist/Album/01 Song.flac,library/Artist/Album/02 Long.flac,library/Artist/Album/03.flac" {
		t.Fatalf("walked %v", keys)
	}
	if err := s.Trash(ctx, ref); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("20060102")
	if fake.Object("kanade/library/Artist/Album/01 Song.flac") != nil || fake.Object("kanade/.kanade-trash/"+today+"/library/Artist/Album/01 Song.flac") == nil {
		t.Fatalf("trashed: %v", fake.Names())
	}
	if err := s.Trash(ctx, ref); err != nil {
		t.Fatalf("trashed again: %v", err)
	}
	if _, err := s.Open(ctx, ref, 0, -1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading a file not there: %v", err)
	}
	if reason, lasting := Explain(func() error { _, err := s.Open(ctx, ref, 0, -1); return err }()); reason != "gone" || !lasting {
		t.Fatalf("explained %s %v", reason, lasting)
	}
	st, _ = s.Check(ctx, 1)
	if st.Usage == nil || st.Usage.Files != 3 || st.Usage.Used == 0 {
		t.Fatalf("usage: %+v", st.Usage)
	}
	fake.Put("kanade/.kanade-trash/20000101/old.flac", []byte("old"))
	fake.AgeUploads(8 * 24 * time.Hour)
	bk := b.(*S3)
	bk.sweptAt = time.Time{}
	if _, err := bk.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.Object("kanade/.kanade-trash/20000101/old.flac") != nil || fake.Object("kanade/.kanade-trash/"+today+"/library/Artist/Album/01 Song.flac") == nil {
		t.Fatalf("swept: %v", fake.Names())
	}

	// Failures: busy (tried again), down, refused.
	tries := 0
	fake.Fail = func(r *http.Request) int {
		if tries++; tries <= 2 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	if got := read(t, s, ref3, 0, 3); got != "0123" || tries != 3 {
		t.Fatalf("busy, then not: %q after %d tries", got, tries)
	}
	fake.Fail = nil
	fake.AccessKey = "rotated"
	if _, err := s.Open(ctx, ref3, 0, 3); !errors.Is(err, ErrAuth) {
		t.Fatalf("keys refused: %v", err)
	}
	if reason, lasting := Explain(func() error { _, err := s.Open(ctx, ref3, 0, 3); return err }()); reason != "store_auth" || !lasting {
		t.Fatalf("explained %s %v", reason, lasting)
	}
	fake.AccessKey = "test-key"
	fake.Close()
	if _, err := bk.Check(ctx); !errors.Is(err, ErrOffline) {
		t.Fatalf("down: %v", err)
	}
	if st, _ := s.Check(ctx, 1); st.State != StateOffline {
		t.Fatalf("recorded %+v", st)
	}
}

// A bucket as a source: listed, read, never written.
func TestS3Source(t *testing.T) {
	ctx := context.Background()
	fake := storagetest.NewS3(t)
	fake.Put("music/Album/01.flac", []byte("one"))
	fake.Put("music/Album/cover.jpg", []byte("jpg"))
	fake.Put("music/Album/", nil) // a folder, as some tools make them
	fake.Put("other/x.flac", []byte("x"))
	fake.Put("music/@eaDir/01.flac/SYNOFILE_THUMB_M.jpg", []byte("thumb")) // a system's folder
	fake.Put("music/Album/._01.flac", []byte("fork"))
	fake.Put("music/@onefive/01.flac", []byte("at")) // music (review #223)
	s, _ := newSet(t)
	st, err := addBucket(t, s, fake, "music", Source, fake.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	if fake.Object("music/.kanade-store") != nil || st.State != StateOK {
		t.Fatalf("a source was written to, or not usable: %+v", st)
	}
	b, _, _ := s.Backend(ctx, st.ID)
	var keys []string
	b.Walk(ctx, func(o Object) error { keys = append(keys, o.Key+"="+o.Version); return nil })
	if len(keys) != 3 || !strings.HasPrefix(keys[0], "@onefive/01.flac=") || !strings.HasPrefix(keys[1], "Album/01.flac=") || !strings.HasPrefix(keys[2], "Album/cover.jpg=") {
		t.Fatalf("walked %v", keys)
	}
	if got := read(t, s, Ref(KindS3, st.ID, "Album/01.flac"), 0, -1); got != "one" {
		t.Fatalf("read %q", got)
	}
	// A server not honouring ranges sends the whole object: what was asked for is cut from it (review #235).
	fake.Put("music/Album/02.flac", []byte("0123456789"))
	fake.NoRanges = true
	for _, c := range []struct {
		start, end int64
		want       string
	}{{0, -1, "0123456789"}, {3, -1, "3456789"}, {3, 5, "345"}, {0, 3, "0123"}} {
		if got := read(t, s, Ref(KindS3, st.ID, "Album/02.flac"), c.start, c.end); got != c.want {
			t.Fatalf("whole object sent for %d-%d: read %q", c.start, c.end, got)
		}
	}
	fake.NoRanges = false
	// One sent with 200 and its Content-Range, as rclone's serve s3 does, is the range.
	fake.RangeStatus = http.StatusOK
	for _, c := range []struct {
		start, end int64
		want       string
	}{{3, -1, "3456789"}, {3, 5, "345"}, {0, 3, "0123"}} {
		if got := read(t, s, Ref(KindS3, st.ID, "Album/02.flac"), c.start, c.end); got != c.want {
			t.Fatalf("a range sent with 200 for %d-%d: read %q", c.start, c.end, got)
		}
	}
	fake.RangeStatus = 0
	if _, err := b.(Writer).Put(ctx, PutFile{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("written: %v", err)
	}
	if err := s.Trash(ctx, Ref(KindS3, st.ID, "Album/01.flac")); err != nil || fake.Object("music/Album/01.flac") == nil {
		t.Fatalf("a source's file let go of: %v", err)
	}
	if _, err := s.Destination(ctx, "1"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("a source as a destination: %v", err)
	}
}

// A bucket that takes requests and never answers them: trying it gives up within tryFor, said so,
// instead of holding the page that waits on it.
func TestS3NoAnswer(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)
	defer func(d time.Duration) { tryFor = d }(tryFor)
	tryFor = 300 * time.Millisecond
	s, _ := newSet(t)
	cfg, _ := json.Marshal(S3Config{Endpoint: srv.URL, Bucket: "music", Prefix: "kanade"})
	sec, _ := json.Marshal(S3Secret{AccessKey: "a", SecretKey: "b"})
	start := time.Now()
	_, err := s.Add(context.Background(), NewStore{Kind: KindS3, Name: "silent", Mode: Managed, Config: cfg, Secret: sec})
	if took := time.Since(start); err == nil || took > 5*time.Second || !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("after %v: %v", took, err)
	}
}

// An R2 endpoint as a bucket's settings show it ends with the bucket's name: taken out when the
// store is added (the bucket field empty or the same), refused when the field names another (a
// store made with it kept its files in that bucket under a folder named as the field said); a path
// on another service stays (one behind a proxy's path).
func TestR2EndpointWithItsBucket(t *testing.T) {
	const acct = "https://0123abcd.r2.cloudflarestorage.com"
	for _, c := range []struct {
		endpoint, bucket  string
		wantEnd, wantName string
		refused           bool
	}{
		{acct + "/music", "", acct, "music", false},
		{acct + "/music/", "music", acct, "music", false},
		{"https://0123abcd.eu.R2.cloudflarestorage.com/music", "", "https://0123abcd.eu.R2.cloudflarestorage.com", "music", false},
		{acct, "music", acct, "music", false},
		{acct + "/test", "music", "", "", true},
		{acct + "/music/kanade", "music", "", "", true},
		{"https://minio.example/s3", "music", "https://minio.example/s3", "music", false},
	} {
		raw, _ := json.Marshal(S3Config{Endpoint: c.endpoint, Bucket: c.bucket, Prefix: "kanade", Region: "auto"})
		got, err := r2Bucket(raw)
		if c.refused {
			var bad *ErrInvalid
			if !errors.As(err, &bad) || !strings.Contains(err.Error(), acct) {
				t.Fatalf("%s with %q: %v", c.endpoint, c.bucket, err)
			}
			continue
		}
		var cfg S3Config
		json.Unmarshal(got, &cfg)
		if err != nil || cfg.Endpoint != c.wantEnd || cfg.Bucket != c.wantName || cfg.Prefix != "kanade" || cfg.Region != "auto" {
			t.Fatalf("%s with %q: %+v %v", c.endpoint, c.bucket, cfg, err)
		}
	}
	// Refused on adding, before any request.
	s, _ := newSet(t)
	cfg, _ := json.Marshal(S3Config{Endpoint: acct + "/test", Bucket: "music", Prefix: "kanade"})
	sec, _ := json.Marshal(S3Secret{AccessKey: "k", SecretKey: "s"})
	var bad *ErrInvalid
	if _, err := s.Add(context.Background(), NewStore{Kind: KindS3, Name: "R2", Mode: Managed, Config: cfg, Secret: sec}); !errors.As(err, &bad) {
		t.Fatalf("added: %v", err)
	}
}
