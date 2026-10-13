package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failPart answers the first upload of part 2 with a refusal, without sending it.
type failPart struct {
	next   http.RoundTripper
	failed bool
}

func (f *failPart) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Query().Get("partNumber") == "2" && !f.failed {
		f.failed = true
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Request: r, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`<Error><Code>AccessDenied</Code><Message>made to fail</Message></Error>`))}, nil
	}
	return f.next.RoundTrip(r)
}

// TestS3Live runs a bucket store against a real S3-compatible service, when KANADE_TEST_S3 says
// which: {"endpoint", "bucket", "prefix", "access_key_id", "secret_access_key"} (an R2 bucket, or
// `rclone serve s3` on a folder). Its signatures are checked by the service itself.
func TestS3Live(t *testing.T) {
	raw := os.Getenv("KANADE_TEST_S3")
	if raw == "" {
		t.Skip("KANADE_TEST_S3 is not set")
	}
	var v struct {
		S3Config
		S3Secret
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer func(w, p int64) { s3Whole, s3Part = w, p }(s3Whole, s3Part)
	s3Whole, s3Part = 6<<20, 5<<20
	s, data := newSet(t)
	s.HTTP = &http.Client{Transport: &failPart{next: http.DefaultTransport}}
	v.S3Config.Prefix += "/kanade-test-" + time.Now().Format("150405")
	cfg, _ := json.Marshal(v.S3Config)
	sec, _ := json.Marshal(v.S3Secret)
	st, err := s.Add(ctx, NewStore{Kind: KindS3, Name: "live", Mode: Managed, Config: cfg, Secret: sec})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := s.Destination(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	// A name with what needs escaping: CJK, spaces, brackets, a plus, an apostrophe.
	p, sha := file(t, data, "a.flac", "fLaC-0123456789")
	ref, err := dest.Put(ctx, PutFile{Path: p, Dir: "library/坂本真綾/Album (Deluxe) [2024]+", Name: "01 It's ~ok!.flac", Size: 15, SHA256: sha})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, ref, 5, 8); got != "0123" {
		t.Fatalf("range: %q", got)
	}
	big := make([]byte, 12<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	bp := filepath.Join(data, "big.flac")
	os.WriteFile(bp, big, 0o644)
	sum := sha256.Sum256(big)
	ses := &testSessions{}
	f := PutFile{Path: bp, Dir: "library/坂本真綾/Album (Deluxe) [2024]+", Name: "02 大きい.flac", Size: int64(len(big)), SHA256: hex.EncodeToString(sum[:]), Sessions: ses}
	if _, err := dest.Put(ctx, f); !errors.Is(err, ErrAuth) {
		t.Fatalf("part 2 refused: %v", err)
	}
	if got := ses.session(t); len(got.Done) != 1 {
		t.Fatalf("kept %+v", got)
	}
	bigRef, err := dest.Put(ctx, f)
	if err != nil {
		t.Fatalf("resumed: %v", err)
	}
	rc, err := s.Open(ctx, bigRef, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	back, _ := io.ReadAll(rc)
	rc.Close()
	if sha256.Sum256(back) != sum {
		t.Fatal("the large file read back is not what was sent")
	}
	if again, err := dest.Put(ctx, f); err != nil || again != bigRef {
		t.Fatalf("the same again: %q %v", again, err)
	}
	b, _, _ := s.Backend(ctx, st.ID)
	var keys []string
	if err := b.Walk(ctx, func(o Object) error { keys = append(keys, o.Key); return nil }); err != nil || len(keys) != 2 {
		t.Fatalf("walked %v %v", keys, err)
	}
	for _, r := range []string{ref, bigRef} {
		if err := s.Trash(ctx, r); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Stat(ctx, r); !errors.Is(err, ErrNotFound) {
			t.Fatalf("still there: %v", err)
		}
	}
	keys = nil
	b.Walk(ctx, func(o Object) error { keys = append(keys, o.Key); return nil })
	if len(keys) != 0 {
		t.Fatalf("left %v", keys)
	}
	if u, err := b.Check(ctx); err != nil || u.Files != 0 || u.Used == 0 {
		t.Fatalf("usage %+v %v", u, err)
	}
	t.Logf("trash and mark left in %s/%s", v.Bucket, v.S3Config.Prefix)
}
