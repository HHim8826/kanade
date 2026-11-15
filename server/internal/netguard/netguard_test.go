package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	never := []string{"127.0.0.1", "127.8.9.10", "::1", "0.0.0.0", "::", "0.1.2.3", "169.254.169.254", "fe80::1",
		"224.0.0.1", "ff02::1", "255.255.255.255", "240.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::7f00:1"}
	local := []string{"10.0.0.1", "172.16.5.4", "172.31.255.255", "192.168.1.10", "100.64.0.1", "100.127.255.254",
		"198.18.0.1", "fc00::1", "fd12:3456::1", "::ffff:192.168.0.1", "64:ff9b::c0a8:1"}
	public := []string{"1.1.1.1", "8.8.8.8", "172.32.0.1", "100.128.0.1", "2606:4700:4700::1111", "64:ff9b::808:808"}
	for _, allow := range []bool{false, true} {
		AllowLocal(allow)
		for _, s := range never {
			if err := Check(netip.MustParseAddr(s)); !errors.Is(err, ErrBlocked) {
				t.Errorf("local allowed %v: %s let through", allow, s)
			}
		}
		for _, s := range local {
			if err := Check(netip.MustParseAddr(s)); (err == nil) != allow {
				t.Errorf("local allowed %v: %s gave %v", allow, s, err)
			}
		}
		for _, s := range public {
			if err := Check(netip.MustParseAddr(s)); err != nil {
				t.Errorf("local allowed %v: %s gave %v", allow, s, err)
			}
		}
	}
	AllowLocal(false)
}

// A name with any refused address is refused, even beside public ones; a name that cannot be
// resolved is an error, not a pass.
func TestNames(t *testing.T) {
	defer func(l func(context.Context, string, string) ([]netip.Addr, error)) { lookup = l }(lookup)
	names := map[string][]string{"public.test": {"93.184.215.14"}, "mixed.test": {"93.184.215.14", "10.1.2.3"},
		"meta.test": {"169.254.169.254"}, "empty.test": {}}
	lookup = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		ss, ok := names[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var ips []netip.Addr
		for _, s := range ss {
			ips = append(ips, netip.MustParseAddr(s))
		}
		return ips, nil
	}
	ctx := context.Background()
	if ips, err := resolve(ctx, "public.test"); err != nil || len(ips) != 1 {
		t.Fatalf("public: %v %v", ips, err)
	}
	for _, h := range []string{"mixed.test", "meta.test"} {
		if _, err := resolve(ctx, h); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: %v", h, err)
		}
	}
	for _, h := range []string{"empty.test", "missing.test"} {
		if _, err := resolve(ctx, h); err == nil {
			t.Errorf("%s resolved", h)
		}
	}
	if _, err := resolve(ctx, "[::1]"); !errors.Is(err, ErrBlocked) {
		t.Errorf("[::1]: %v", err)
	}
}

// The client does not reach a server on this machine, not even by a redirect from one it may
// reach; through a proxy on this machine it may go, and the proxy is told nothing it should not.
func TestClient(t *testing.T) {
	here := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer here.Close()
	if _, err := Client(5 * time.Second).Get(here.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("this machine: %v", err)
	}
	AllowLocal(true) // loopback stays refused when the local network is allowed
	if _, err := Client(5 * time.Second).Get(here.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("this machine, local network allowed: %v", err)
	}
	AllowLocal(false)

	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.String())
		w.Write([]byte("via proxy"))
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	defer func(p func(*http.Request) (*url.URL, error)) { envProxy = p }(envProxy)
	pu, _ := url.Parse(proxy.URL)
	envProxy = http.ProxyURL(pu)
	defer func(l func(context.Context, string, string) ([]netip.Addr, error)) { lookup = l }(lookup)
	lookup = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host == "inside.test" {
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}
	c := Client(5 * time.Second)
	resp, err := c.Get("http://feeds.test/rss")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	resp.Body.Close()
	if _, err := c.Get("http://inside.test/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a private host through the proxy: %v", err)
	}
	if _, err := c.Get("http://169.254.169.254/latest/meta-data/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("metadata through the proxy: %v", err)
	}
	if len(asked) != 1 || asked[0] != "http://feeds.test/rss" {
		t.Fatalf("the proxy was asked %v", asked)
	}
}
