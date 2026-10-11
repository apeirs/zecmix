// Package lwd talks to a lightwalletd-compatible server (lightwalletd, Zaino)
// over gRPC, with every connection carried by the Nym mixnet.
package lwd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"

	"github.com/apeirs/zecmix/mixnet"
	"github.com/apeirs/zecmix/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Server is a public lightwalletd endpoint.
type Server struct {
	Addr    string // host:port
	Network string // "main" or "test"
	Note    string
}

// KnownServers are public endpoints on ports the Nym exit policy allows
// (443 and 9067 are both open; 9067 falls in the 8081-9080 range).
var KnownServers = []Server{
	{"zec.rocks:443", "main", "Zec.rocks (Zaino), anycast default"},
	{"na.zec.rocks:443", "main", "Zec.rocks North America"},
	{"eu.zec.rocks:443", "main", "Zec.rocks Europe"},
	{"ap.zec.rocks:443", "main", "Zec.rocks Asia-Pacific"},
	{"sa.zec.rocks:443", "main", "Zec.rocks South America"},
	{"testnet.zec.rocks:443", "test", "Zec.rocks testnet"},
}

// DefaultServer is used when no --server is given.
const DefaultServer = "zec.rocks:443"

// Options configure Dial.
type Options struct {
	Plaintext bool           // h2c without TLS (only for local test servers)
	RootCAs   *x509.CertPool // nil = system roots
}

// MaxMsgSize: compact blocks from the 2022-2023 sandblasting era can be large.
const MaxMsgSize = 64 << 20

// Dial returns a gRPC client for server ("host:port") whose single HTTP/2
// connection runs inside a SOCKS5 stream through the Nym mixnet, with TLS end
// to end to the server.
//
// The target uses the "passthrough" resolver: gRPC's default "dns" resolver
// would look the hostname up with the *local* resolver, leaking which server
// you use to your DNS provider. With passthrough the name goes, unresolved,
// to the mixnet dialer.
func Dial(d *mixnet.Dialer, server string, opt Options) (*grpc.ClientConn, walletrpc.CompactTxStreamerClient, error) {
	if u, err := url.Parse(server); err == nil && u.Scheme == "http" {
		return dialLocalGateway(u.Host)
	}
	addr, err := mixnet.HostPort(server, 443)
	if err != nil {
		return nil, nil, err
	}
	host, _, _ := net.SplitHostPort(addr)
	creds := credentials.NewTLS(&tls.Config{ServerName: host, RootCAs: opt.RootCAs, MinVersion: tls.VersionTLS12})
	if opt.Plaintext {
		creds = insecure.NewCredentials()
	}
	cc, err := grpc.NewClient("passthrough:///"+addr,
		grpc.WithContextDialer(func(ctx context.Context, a string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", a)
		}),
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMsgSize)),
		grpc.WithUserAgent("zecmix"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("grpc client for %s: %w", addr, err)
	}
	return cc, walletrpc.NewCompactTxStreamerClient(cc), nil
}

// dialLocalGateway connects to a zecmix gateway on loopback (plaintext h2c).
// It is the only non-mixnet connection lwd ever makes, and only to 127.0.0.1/::1:
// the gateway itself carries the call over the mixnet.
func dialLocalGateway(hostport string) (*grpc.ClientConn, walletrpc.CompactTxStreamerClient, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, nil, errors.New("http:// (plaintext) servers are only allowed on loopback, i.e. a local zecmix gateway; use host:port for TLS over the mixnet")
	}
	cc, err := grpc.NewClient("passthrough:///"+hostport,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMsgSize)))
	if err != nil {
		return nil, nil, err
	}
	return cc, walletrpc.NewCompactTxStreamerClient(cc), nil
}
