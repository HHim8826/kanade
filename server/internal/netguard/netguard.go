// Package netguard keeps fetches of addresses that come from outside — a feed, the links in it, a
// link pasted to download — off this machine and the networks around it: loopback (aria2's RPC,
// Kanade itself), link-local (a cloud's metadata service at 169.254.169.254), private and shared
// (CGNAT) ranges, and addresses that are no one's. Private and shared ranges can be let through for
// a NAS on the local network (the downloads setting "local_links"); the others never are.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ErrBlocked is an address a fetch from outside may not reach.
var ErrBlocked = errors.New("the link leads to this machine or a private network")

var local atomic.Bool

// AllowLocal lets fetches reach private and shared (CGNAT) addresses, or stops them again.
func AllowLocal(on bool) { local.Store(on) }

var (
	shared  = netip.MustParsePrefix("100.64.0.0/10")  // carrier-grade NAT (RFC 6598)
	bench   = netip.MustParsePrefix("198.18.0.0/15")  // benchmarking (RFC 2544)
	this    = netip.MustParsePrefix("0.0.0.0/8")      // "this network"
	reserve = netip.MustParsePrefix("240.0.0.0/4")    // reserved, and the broadcast address
	nat64   = netip.MustParsePrefix("64:ff9b::/96")   // IPv4 in IPv6, checked as the IPv4 inside
	sixFour = netip.MustParsePrefix("64:ff9b:1::/48") // local-use NAT64 (RFC 8215)
)

// Check says whether a fetch from outside may reach ip.
func Check(ip netip.Addr) error {
	ip = ip.Unmap()
	if nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte(b[12:]))
	}
	switch {
	case !ip.IsValid(), ip.IsLoopback(), ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast(), this.Contains(ip), reserve.Contains(ip):
		return ErrBlocked
	case ip.IsPrivate(), shared.Contains(ip), bench.Contains(ip), sixFour.Contains(ip):
		if !local.Load() {
			return fmt.Errorf("%w (local network addresses can be allowed in the download settings)", ErrBlocked)
		}
	}
	return nil
}

// lookup is the resolver's, replaced in tests.
var lookup = net.DefaultResolver.LookupNetIP

// resolve gives the addresses of host, all of which must be allowed: a name that has any address
// on this machine or a private network is refused, even if it has public ones too.
func resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{ip}, Check(ip)
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}
	for _, ip := range ips {
		if err := Check(ip); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// Dial connects to addr, refusing any address Check refuses. It dials what it checked, so a name
// that resolves again to another address in between (DNS rebinding) does not get past it.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	for _, ip := range ips {
		var c net.Conn
		if c, err = d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port)); err == nil {
			return c, nil
		}
	}
	return nil, err
}

// Transport is an HTTP transport for fetches from outside. Through a proxy (HTTP_PROXY,
// HTTPS_PROXY) it is the proxy that connects, to whatever it resolves: each request's host is
// checked before it is sent, and the proxy itself, which may well be on this machine, is let be.
func Transport() *http.Transport {
	proxies := map[string]bool{}
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		if u, err := url.Parse(os.Getenv(k)); err == nil && u.Host != "" {
			proxies[hostPort(u)] = true
		}
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if proxies[addr] {
			return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, addr)
		}
		return Dial(ctx, network, addr)
	}
	return &http.Transport{DialContext: dial, Proxy: checkedProxy, ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: 15 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 10}
}

// envProxy is http.ProxyFromEnvironment, which reads the environment once; replaced in tests.
var envProxy = http.ProxyFromEnvironment

// checkedProxy is the proxy the environment gives, after checking the request's own host when a
// proxy is to fetch it.
func checkedProxy(req *http.Request) (*url.URL, error) {
	p, err := envProxy(req)
	if err != nil || p == nil {
		return p, err
	}
	if _, err := resolve(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	return p, nil
}

func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// Client is an HTTP client for fetches from outside, with the given time limit for each request.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport()}
}
