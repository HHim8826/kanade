// Package clientip tells the address a request comes from, for the login throttle and the request
// log (review #148). Behind a proxy every request arrives from the proxy, and the client's address
// is in a header the proxy adds; but a client can send those headers itself, and some proxies pass
// them on. So the headers are believed only as the settings say (trusted_proxy), and only when the
// request comes from a proxy named there; by default only the connection's own address counts.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Policy is whose forwarding headers to believe. The zero Policy believes none.
type Policy struct {
	cloudflare bool           // the client is in CF-Connecting-IP (a Cloudflare Tunnel, or a site behind Cloudflare)
	proxies    []netip.Prefix // the proxies in front; with none set, cloudflare means the tunnel on this machine
}

// Parse reads the trusted_proxy setting: empty (no proxy is believed), or a comma-separated list of
//   - cloudflare: the client's address is CF-Connecting-IP, which Cloudflare sets itself;
//   - loopback: a proxy on this machine (Caddy, Nginx): the client's address is the last one in
//     X-Forwarded-For that is not a trusted proxy;
//   - addresses or CIDR ranges of the proxies in front, the same way.
//
// cloudflare alone means cloudflared runs on this machine.
func Parse(v string) (Policy, error) {
	var p Policy
	for _, f := range strings.Split(v, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		switch {
		case f == "" || f == "none":
		case f == "cloudflare":
			p.cloudflare = true
		case f == "loopback":
			p.proxies = append(p.proxies, netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"))
		case strings.Contains(f, "/"):
			n, err := netip.ParsePrefix(f)
			if err != nil {
				return Policy{}, fmt.Errorf("trusted_proxy: %q is not an address range like 10.0.0.0/8", f)
			}
			p.proxies = append(p.proxies, n.Masked())
		default:
			a, err := netip.ParseAddr(f)
			if err != nil {
				return Policy{}, fmt.Errorf("trusted_proxy: want cloudflare, loopback, or the proxies' addresses; not %q", f)
			}
			p.proxies = append(p.proxies, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		}
	}
	if p.cloudflare && len(p.proxies) == 0 {
		p.proxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	}
	return p, nil
}

// Set reports whether any proxy is believed.
func (p Policy) Set() bool { return len(p.proxies) > 0 }

func (p Policy) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, n := range p.proxies {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

// Of is the address r comes from.
func (p Policy) Of(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !p.trusted(peer) {
		return host
	}
	if p.cloudflare {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
			return a.Unmap().String()
		}
		return host // not through Cloudflare: someone on this machine
	}
	// From the right, past the proxies: what is left of that was written by the client.
	hops := forwarded(r)
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		if !p.trusted(a) {
			return a.Unmap().String()
		}
	}
	return host
}

// Forwarded reports whether r carries a forwarding header, from a peer on this machine: a proxy
// that settings do not name.
func Forwarded(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback() && (r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("X-Forwarded-For") != "")
}

func forwarded(r *http.Request) []string {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			hops = append(hops, strings.TrimSpace(h))
		}
	}
	return hops
}
