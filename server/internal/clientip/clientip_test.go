package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestOf(t *testing.T) {
	type req struct {
		peer, cf string
		xff      []string
	}
	cases := []struct {
		policy string
		r      req
		want   string
	}{
		// By default no header is believed, from anywhere (review #148).
		{"", req{"127.0.0.1:5000", "203.0.113.9", nil}, "127.0.0.1"},
		{"", req{"127.0.0.1:5000", "", []string{"203.0.113.9"}}, "127.0.0.1"},
		{"", req{"198.51.100.7:5000", "203.0.113.9", nil}, "198.51.100.7"},

		// cloudflare: CF-Connecting-IP from cloudflared on this machine.
		{"cloudflare", req{"127.0.0.1:5000", "203.0.113.9", nil}, "203.0.113.9"},
		{"cloudflare", req{"[::1]:5000", "2001:db8::1", nil}, "2001:db8::1"},
		{"cloudflare", req{"198.51.100.7:5000", "203.0.113.9", nil}, "198.51.100.7"}, // straight to the port
		{"cloudflare", req{"127.0.0.1:5000", "", []string{"203.0.113.9"}}, "127.0.0.1"},
		{"cloudflare", req{"127.0.0.1:5000", "not an address", nil}, "127.0.0.1"},
		// Behind Cloudflare, a proxy at 10.0.0.2.
		{"cloudflare, 10.0.0.2", req{"10.0.0.2:5000", "203.0.113.9", nil}, "203.0.113.9"},
		{"cloudflare, 10.0.0.2", req{"127.0.0.1:5000", "203.0.113.9", nil}, "127.0.0.1"},

		// loopback: Caddy or Nginx on this machine, which add the address they saw on the right.
		{"loopback", req{"127.0.0.1:5000", "", []string{"203.0.113.9"}}, "203.0.113.9"},
		{"loopback", req{"127.0.0.1:5000", "", []string{"192.0.2.1, 203.0.113.9"}}, "203.0.113.9"}, // the client wrote 192.0.2.1
		{"loopback", req{"127.0.0.1:5000", "", []string{"192.0.2.1", "203.0.113.9"}}, "203.0.113.9"},
		{"loopback", req{"127.0.0.1:5000", "", []string{"192.0.2.1, 127.0.0.1"}}, "192.0.2.1"}, // a proxy chain on this machine
		{"loopback", req{"127.0.0.1:5000", "192.0.2.5", []string{"203.0.113.9"}}, "203.0.113.9"},
		{"loopback", req{"127.0.0.1:5000", "", nil}, "127.0.0.1"},
		{"loopback", req{"127.0.0.1:5000", "", []string{"junk, 203.0.113.9"}}, "203.0.113.9"},
		{"loopback", req{"127.0.0.1:5000", "", []string{"203.0.113.9, junk"}}, "127.0.0.1"},
		{"loopback", req{"198.51.100.7:5000", "", []string{"203.0.113.9"}}, "198.51.100.7"},
		// Proxies by range.
		{"10.0.0.0/8", req{"10.1.2.3:5000", "", []string{"192.0.2.1, 203.0.113.9, 10.9.9.9"}}, "203.0.113.9"},
		{"10.0.0.0/8", req{"127.0.0.1:5000", "", []string{"203.0.113.9"}}, "127.0.0.1"},
	}
	for _, c := range cases {
		p, err := Parse(c.policy)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.r.peer
		if c.r.cf != "" {
			r.Header.Set("CF-Connecting-IP", c.r.cf)
		}
		for _, v := range c.r.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := p.Of(r); got != c.want {
			t.Errorf("%q %+v: %s, want %s", c.policy, c.r, got, c.want)
		}
	}
}

func TestParse(t *testing.T) {
	for _, v := range []string{"", "none", "cloudflare", "Cloudflare", "loopback", "10.0.0.0/8, 192.168.1.2", "::1, cloudflare"} {
		if _, err := Parse(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"nginx", "10.0.0.0/33", "1.2.3"} {
		if _, err := Parse(v); err == nil {
			t.Errorf("%q passed", v)
		}
	}
	if p, _ := Parse(""); p.Set() {
		t.Error("none is set")
	}
}
