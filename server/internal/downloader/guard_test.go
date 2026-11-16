package downloader

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/netguard"
)

// The tests' web servers are on this machine, which links from outside may not reach.
func TestMain(m *testing.M) {
	web = http.DefaultClient
	os.Exit(m.Run())
}

// A link to this machine is not fetched, for a download or for its torrent again (review T1).
func TestLinksStayOffThisMachine(t *testing.T) {
	defer func(c *http.Client) { web = c }(web)
	web = &http.Client{Transport: netguard.Transport()}
	hit := false
	here := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer here.Close()
	if _, _, err := probe(context.Background(), here.URL+"/album.zip"); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("probe: %v", err)
	}
	if _, err := fetchTorrent(context.Background(), here.URL+"/a.torrent"); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("fetchTorrent: %v", err)
	}
	if hit {
		t.Fatal("the server on this machine was asked")
	}
}

// aria2 fetches a direct download's file itself: through Kanade's proxy, so that a link Kanade
// checked cannot send it on to this machine by a redirect (review T1). A file it may reach still
// downloads.
func TestDirectRedirectKeptOff(t *testing.T) {
	var mu sync.Mutex
	hit := false
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = true
		mu.Unlock()
		http.ServeContent(w, r, "a.ogg", time.Time{}, bytes.NewReader(testdata(t, "tone.ogg")))
	}))
	defer private.Close()
	song := testdata(t, "tone.ogg")
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Kanade's look at the link is answered; aria2, sent on to this machine.
		if strings.HasPrefix(r.UserAgent(), "aria2") && r.URL.Path == "/away.ogg" {
			http.Redirect(w, r, private.URL+"/a.ogg", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "audio/ogg")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(song))
	}))
	defer public.Close()
	ctx, e := newDirectEnvWith(t, func(ctx context.Context, a *Aria2) {
		p, err := netguard.NewProxy(nil)
		if err != nil {
			t.Fatal(err)
		}
		p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) { // the test's "public" server is let through
			if addr == public.Listener.Addr().String() {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			return netguard.Dial(ctx, network, addr)
		}
		go p.Serve(ctx)
		a.UseProxy(p.Addr(), p.User, p.Pass)
	})
	away, err := e.svc.Add(ctx, public.URL+"/away.ogg", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the download to fail", 30*time.Second, func() bool {
		v, _ := e.svc.Get(ctx, away)
		return v.State == StateFailed
	})
	mu.Lock()
	if hit {
		t.Fatal("aria2 reached the server on this machine")
	}
	mu.Unlock()
	there, err := e.svc.Add(ctx, public.URL+"/there.ogg", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	e.imported(t, ctx, there)
}
