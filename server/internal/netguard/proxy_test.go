package netguard

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A program going through the proxy reaches what Dial lets it reach, http:// as it asks and
// https:// through a tunnel, and nothing else; and only with the password (review T1).
func TestProxy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	hits := map[string]int{}
	serve := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
			if r.Header.Get("Proxy-Authorization") != "" {
				t.Errorf("%s was told the proxy's password", name)
			}
			io.WriteString(w, name)
		}
	}
	public := httptest.NewServer(serve("public"))
	defer public.Close()
	secure := httptest.NewTLSServer(serve("secure"))
	defer secure.Close()
	private := httptest.NewServer(serve("private"))
	defer private.Close()
	// The tests' servers are all on this machine: "public" and "secure" are let through as if
	// they were elsewhere, the rest is Dial's to say.
	outside := map[string]bool{public.Listener.Addr().String(): true, secure.Listener.Addr().String(): true}
	p, err := NewProxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if outside[addr] {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return Dial(ctx, network, addr)
	}
	go p.Serve(ctx)
	through := func(user *url.Userinfo) *http.Client {
		u := &url.URL{Scheme: "http", Host: p.Addr(), User: user}
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{RootCAs: secure.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	get := func(c *http.Client, u string) (int, string, error) {
		resp, err := c.Get(u)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}
	ok := through(url.UserPassword(p.User, p.Pass))
	if code, body, err := get(ok, public.URL+"/a.flac"); err != nil || code != 200 || body != "public" {
		t.Fatalf("http: %d %q %v", code, body, err)
	}
	if code, body, err := get(ok, secure.URL+"/a.flac"); err != nil || code != 200 || body != "secure" {
		t.Fatalf("https: %d %q %v", code, body, err)
	}
	if code, _, err := get(ok, private.URL+"/a.flac"); err != nil || code != http.StatusForbidden {
		t.Fatalf("http to this machine: %d %v", code, err)
	}
	if _, _, err := get(ok, strings.Replace(private.URL, "http://", "https://", 1)); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("a tunnel to this machine: %v", err)
	}
	for _, user := range []*url.Userinfo{nil, url.UserPassword(p.User, "wrong")} {
		if code, _, err := get(through(user), public.URL); err != nil || code != http.StatusProxyAuthRequired {
			t.Fatalf("password %v: %d %v", user, code, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["private"] != 0 || hits["public"] != 1 || hits["secure"] != 1 {
		t.Fatalf("hits %v", hits)
	}
}
