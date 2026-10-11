// Package gateway is a local lightwalletd endpoint that forwards every gRPC
// call to a public lightwalletd server through the Nym mixnet.
//
// A wallet (zingo-cli, zcash-devtool, any lightwalletd client that accepts a
// custom server URL) connects to 127.0.0.1. Each call is proxied at the HTTP/2
// level (gRPC streams, trailers and flow control are preserved) over one TLS
// connection to the upstream server; that TLS connection runs inside a SOCKS5
// stream through nym-socks5-client, the mixnet and an exit gateway. The
// upstream server sees the exit gateway's IP, never the wallet's.
package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/apeirs/zecmix/mixnet"
	"golang.org/x/net/http2"
)

// Gateway proxies gRPC calls to Upstream through the mixnet.
type Gateway struct {
	Upstream string // host:port of the public lightwalletd (TLS)
	Dialer   *mixnet.Dialer
	Log      func(format string, args ...any)
	RootCAs  *x509.CertPool // nil = system roots (tests override)

	// UpstreamPlaintext speaks h2c to the upstream (tests only).
	UpstreamPlaintext bool

	calls atomic.Int64
	proxy *httputil.ReverseProxy
}

func (g *Gateway) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log(format, args...)
	}
}

// hop-by-hop or identifying headers a client might add; never forwarded.
var stripHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Via"}

// Handler returns the http.Handler that serves the local gRPC endpoint.
func (g *Gateway) Handler() http.Handler {
	host, _, err := net.SplitHostPort(g.Upstream)
	if err != nil {
		panic(fmt.Sprintf("gateway: bad upstream %q: %v", g.Upstream, err))
	}
	authority := g.Upstream
	if strings.HasSuffix(authority, ":443") {
		authority = host
	}
	tr := &http2.Transport{
		// Keep the single mixnet-carried connection healthy; mixnet RTT is
		// seconds, so be patient.
		ReadIdleTimeout: 45 * time.Second,
		PingTimeout:     30 * time.Second,
	}
	scheme := "https"
	if g.UpstreamPlaintext {
		scheme = "http"
		tr.AllowHTTP = true
		tr.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return g.Dialer.DialContext(ctx, "tcp", addr)
		}
	} else {
		tr.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return g.Dialer.DialTLS(ctx, addr, &tls.Config{
				ServerName: host,
				NextProtos: []string{"h2"},
				MinVersion: tls.VersionTLS12,
				RootCAs:    g.RootCAs,
			})
		}
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = g.Upstream
			pr.Out.Host = authority
			for _, h := range stripHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("User-Agent", "grpc-go")
		},
		Transport:     tr,
		FlushInterval: -1, // stream every message as it arrives (GetBlockRange)
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.logf("call %s FAILED upstream via mixnet: %v", r.URL.Path, err)
			grpcError(w, 14, "zecmix: upstream unreachable over the Nym mixnet: "+err.Error())
		},
	}
	return http.HandlerFunc(g.serve)
}

// grpcError writes a gRPC "trailers-only" error response.
func grpcError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Grpc-Status", fmt.Sprint(code))
	w.Header().Set("Grpc-Message", url.PathEscape(msg))
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor != 2 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		http.Error(w, "zecmix gateway: gRPC (HTTP/2) only; point a lightwalletd client here", http.StatusUnsupportedMediaType)
		return
	}
	n := g.calls.Add(1)
	t0 := time.Now()
	cw := &countingWriter{ResponseWriter: w}
	g.proxy.ServeHTTP(cw, r)
	status := cw.Header().Get("Grpc-Status")
	if status == "" {
		status = cw.Header().Get(http.TrailerPrefix + "Grpc-Status")
	}
	if status == "" {
		status = "?"
	}
	g.logf("call #%d %s -> %s via mixnet: grpc-status=%s, %d bytes, %.2fs", n, r.URL.Path, g.Upstream, status, cw.n, time.Since(t0).Seconds())
}

// Calls returns how many calls the gateway has proxied.
func (g *Gateway) Calls() int64 { return g.calls.Load() }

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Server builds the local HTTP/2 server. With tlsCfg == nil it serves h2c
// (plaintext HTTP/2 with prior knowledge, what gRPC clients use for http://).
func (g *Gateway) Server(tlsCfg *tls.Config) *http.Server {
	var p http.Protocols
	if tlsCfg == nil {
		p.SetUnencryptedHTTP2(true)
	} else {
		p.SetHTTP2(true)
	}
	return &http.Server{
		Handler:           g.Handler(),
		Protocols:         &p,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 30 * time.Second,
	}
}

// IsLoopback reports whether a listen address only binds loopback.
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
