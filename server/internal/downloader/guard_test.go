package downloader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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
