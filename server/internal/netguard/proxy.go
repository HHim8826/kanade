package netguard

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"
)

// Proxy is an HTTP proxy on this machine for a program that fetches links from outside on its own
// (aria2: a direct download's file, its redirects, a torrent's trackers and web seeds), so that
// those connections are Dial's as well: Kanade checking a link before handing it over leaves out
// where a redirect, or the name resolved again, then leads (review T1). It listens on loopback and
// asks for a password, so that it is no one else's way out.
type Proxy struct {
	User, Pass string
	Log        *slog.Logger
	// Dial connects; nil is this package's Dial (tests let their servers on this machine through).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	ln   net.Listener
	auth string // the Proxy-Authorization header it takes
	web  *httputil.ReverseProxy
}

// NewProxy listens on a free loopback port, with a new password.
func NewProxy(log *slog.Logger) (*Proxy, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{User: "kanade", Pass: hex.EncodeToString(raw), Log: log, ln: ln}
	p.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(p.User+":"+p.Pass))
	p.web = &httputil.ReverseProxy{
		// The request as asked, to where it asked; no X-Forwarded-For.
		Rewrite:   func(pr *httputil.ProxyRequest) { pr.Out.URL, pr.Out.Host = pr.In.URL, pr.In.Host },
		Transport: &http.Transport{DialContext: p.connect, ResponseHeaderTimeout: time.Minute, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 10},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, err.Error(), p.refused(r.URL.Host, err))
		},
	}
	return p, nil
}

func (p *Proxy) connect(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	return Dial(ctx, network, addr)
}

// Addr is where it listens, host:port.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Serve answers until ctx ends.
func (p *Proxy) Serve(ctx context.Context) {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	srv.Serve(p.ln)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Proxy-Authorization")), []byte(p.auth)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="kanade"`)
		http.Error(w, "", http.StatusProxyAuthRequired)
		return
	}
	switch {
	case r.Method == http.MethodConnect:
		p.tunnel(w, r)
	case r.URL.Scheme == "http" && r.URL.Host != "":
		p.web.ServeHTTP(w, r)
	default:
		http.Error(w, "http:// links, or CONNECT for https://", http.StatusBadRequest)
	}
}

// refused is the status for a connection that was not made, said in the log when it was refused.
func (p *Proxy) refused(host string, err error) int {
	if !errors.Is(err, ErrBlocked) {
		return http.StatusBadGateway
	}
	if p.Log != nil {
		p.Log.Warn("a download was kept off this machine or the local network", "host", host, "err", err)
	}
	return http.StatusForbidden
}

// tunnel joins the client to the address it asked for (CONNECT, for https://).
func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request) {
	up, err := p.connect(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), p.refused(r.Host, err))
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "no tunnel", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		conn.Close()
		up.Close()
		return
	}
	pipe(conn, rw.Reader, up)
}

// pipe copies both ways until either side is done, then closes both. What the client sent past
// the request (already read into br) goes first.
func pipe(client net.Conn, br *bufio.Reader, up net.Conn) {
	var once sync.Once
	done := func() {
		once.Do(func() {
			client.Close()
			up.Close()
		})
	}
	go func() {
		io.Copy(up, br)
		done()
	}()
	io.Copy(client, up)
	done()
}
