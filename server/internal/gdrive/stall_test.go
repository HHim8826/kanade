package gdrive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A token endpoint that never answers (review #149) holds up neither Status nor anyone but those
// needing the token, and only for refreshTimeout; one refresh serves everyone waiting.
func TestRefreshHangingHoldsNothingUp(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	old, oldTimeout := tokenEndpoint, refreshTimeout
	tokenEndpoint, refreshTimeout = srv.URL, 500*time.Millisecond
	defer func() { tokenEndpoint, refreshTimeout = old, oldTimeout }()
	c := newClient(t)
	c.ImportToken(ctx, Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour)})

	errs := make(chan error, 3)
	for range 3 {
		go func() { _, err := c.About(context.Background()); errs <- err }()
	}
	time.Sleep(100 * time.Millisecond)
	short, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	t0 := time.Now()
	if st, err := c.Status(short); err != nil || !st.Connected {
		t.Fatalf("status: %+v %v", st, err)
	}
	if d := time.Since(t0); d > 200*time.Millisecond {
		t.Fatalf("status took %v", d)
	}
	for range 3 {
		if err := <-errs; err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a hanging refresh: %v", err)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("%d refreshes for 3 at once", n)
	}
}

// A body Drive stops sending in the middle fails, instead of waiting for ever; reading slowly is
// not stopping.
func TestStalledBodyFails(t *testing.T) {
	ctx := context.Background()
	old := stallAfter
	stallAfter = 300 * time.Millisecond
	defer func() { stallAfter = old }()
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2000")
		w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		if r.URL.Query().Get("alt") == "media" && r.Header.Get("Range") == "bytes=0-" {
			select { // stops
			case <-hold:
			case <-r.Context().Done():
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
		w.Write(make([]byte, 1000))
	}))
	defer srv.Close()
	defer close(hold)
	c := newClient(t)
	c.UseHTTPClient(&http.Client{Transport: rewrite{srv.URL}})
	c.ImportToken(ctx, Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)})

	resp, err := c.OpenRange(ctx, "f1", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("stopped body: %v", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("gave up after %v", d)
	}

	resp, err = c.OpenRange(ctx, "f1", 0, 1999)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 500)
	n := 0
	for {
		time.Sleep(200 * time.Millisecond) // the reader taking its time, two times over the limit
		k, err := resp.Body.Read(buf)
		n += k
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("slow reader: %v", err)
		}
	}
	if n != 2000 {
		t.Fatalf("read %d", n)
	}
}

// A chunk Drive stops taking fails too.
func TestStalledUploadFails(t *testing.T) {
	ctx := context.Background()
	old := stallAfter
	stallAfter = 300 * time.Millisecond
	defer func() { stallAfter = old }()
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold // reads nothing of the body
	}))
	defer srv.Close()
	defer close(hold)
	c := newClient(t)
	c.UseHTTPClient(&http.Client{Transport: rewrite{srv.URL}})
	c.ImportToken(ctx, Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)})
	path := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(path, make([]byte, 64<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	done := make(chan error, 1)
	go func() { _, _, err := c.putChunk(ctx, srv.URL+"/upload", f, 0, 64<<20, 64<<20); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStalled) {
			t.Fatalf("stopped upload: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting")
	}
}
