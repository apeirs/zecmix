// Package fakes provides offline stand-ins for the mixnet pieces: a SOCKS5
// proxy that records every CONNECT, a DoH resolver and a lightwalletd server.
package fakes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/apeirs/zecmix/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SOCKS is a fake nym-socks5-client. Routes maps a requested "host:port"
// (exactly as the client sent it) to a local address to splice to.
type SOCKS struct {
	Addr string

	mu       sync.Mutex
	Requests []Request
	Routes   map[string]string
	ln       net.Listener
}

// Request is one CONNECT the proxy received.
type Request struct {
	ATYP byte // 1 = IPv4, 3 = domain name, 4 = IPv6
	Host string
	Port int
}

func (r Request) String() string { return fmt.Sprintf("%s:%d(atyp %d)", r.Host, r.Port, r.ATYP) }

// NewSOCKS starts the proxy on 127.0.0.1:0.
func NewSOCKS(routes map[string]string) *SOCKS {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	s := &SOCKS{Addr: ln.Addr().String(), Routes: routes, ln: ln}
	go s.serve()
	return s
}

func (s *SOCKS) Close() { s.ln.Close() }

// Seen returns a copy of the CONNECT requests so far.
func (s *SOCKS) Seen() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.Requests...)
}

func (s *SOCKS) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *SOCKS) handle(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 262)
	if _, err := io.ReadFull(c, buf[:3]); err != nil {
		return
	}
	c.Write([]byte{5, 0})
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return
	}
	req := Request{ATYP: buf[3]}
	switch req.ATYP {
	case 1:
		io.ReadFull(c, buf[:4])
		req.Host = net.IP(buf[:4]).String()
	case 4:
		io.ReadFull(c, buf[:16])
		req.Host = net.IP(buf[:16]).String()
	case 3:
		io.ReadFull(c, buf[:1])
		n := int(buf[0])
		io.ReadFull(c, buf[:n])
		req.Host = string(buf[:n])
	}
	io.ReadFull(c, buf[:2])
	req.Port = int(binary.BigEndian.Uint16(buf[:2]))
	s.mu.Lock()
	s.Requests = append(s.Requests, req)
	target, ok := s.Routes[net.JoinHostPort(req.Host, fmt.Sprint(req.Port))]
	s.mu.Unlock()
	if !ok {
		c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}) // host unreachable
		return
	}
	up, err := net.Dial("tcp", target)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
}

// CA is a throwaway certificate authority.
type CA struct {
	Pool *x509.CertPool
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func NewCA() *CA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{Pool: pool, cert: cert, key: key}
}

// Leaf issues a server certificate for the given DNS names / IPs.
func (ca *CA) Leaf(names ...string) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// DoH starts a JSON DNS-over-HTTPS server (TLS cert for ip) answering A
// queries from records. Returns its local address.
func DoH(ca *CA, ip string, records map[string][]string) string {
	ln, _ := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{ca.Leaf(ip)}})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		var ans []map[string]any
		for _, a := range records[strings.TrimSuffix(name, ".")] {
			ans = append(ans, map[string]any{"name": name, "type": 1, "data": a})
		}
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(map[string]any{"Status": 0, "Answer": ans})
	})
	go http.Serve(ln, h)
	return ln.Addr().String()
}

// Lightwalletd is a fake CompactTxStreamer.
type Lightwalletd struct {
	walletrpc.UnimplementedCompactTxStreamerServer
	Tip uint64

	mu  sync.Mutex
	MD  []metadata.MD // incoming metadata per call
	Got [][]byte      // raw txs received by SendTransaction
}

func (l *Lightwalletd) record(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	l.mu.Lock()
	l.MD = append(l.MD, md)
	l.mu.Unlock()
}

// Metadata returns the metadata of every call so far.
func (l *Lightwalletd) Metadata() []metadata.MD {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]metadata.MD(nil), l.MD...)
}

func (l *Lightwalletd) GetLightdInfo(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.LightdInfo, error) {
	l.record(ctx)
	return &walletrpc.LightdInfo{Vendor: "fake", ChainName: "main", BlockHeight: l.Tip, SaplingActivationHeight: 419200}, nil
}

func (l *Lightwalletd) GetLatestBlock(ctx context.Context, _ *walletrpc.ChainSpec) (*walletrpc.BlockID, error) {
	l.record(ctx)
	return &walletrpc.BlockID{Height: l.Tip, Hash: blockHash(l.Tip)}, nil
}

func blockHash(h uint64) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint64(b, h)
	b[31] = 0 // display form starts with zeros like a real block hash
	return b
}

func (l *Lightwalletd) GetBlockRange(r *walletrpc.BlockRange, s grpc.ServerStreamingServer[walletrpc.CompactBlock]) error {
	l.record(s.Context())
	for h := r.Start.Height; h <= r.End.Height; h++ {
		blk := &walletrpc.CompactBlock{Height: h, Hash: blockHash(h), PrevHash: blockHash(h - 1), Time: uint32(1700000000 + h),
			Vtx: []*walletrpc.CompactTx{{Txid: blockHash(h), Actions: []*walletrpc.CompactOrchardAction{{}}}}}
		if err := s.Send(blk); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lightwalletd) SendTransaction(ctx context.Context, tx *walletrpc.RawTransaction) (*walletrpc.SendResponse, error) {
	l.record(ctx)
	l.mu.Lock()
	l.Got = append(l.Got, tx.Data)
	l.mu.Unlock()
	return &walletrpc.SendResponse{ErrorCode: -22, ErrorMessage: "parse error: invalid consensus branch id"}, nil
}

func (l *Lightwalletd) GetTreeState(ctx context.Context, id *walletrpc.BlockID) (*walletrpc.TreeState, error) {
	l.record(ctx)
	if id.Height > l.Tip {
		return nil, status.Error(codes.OutOfRange, "height beyond tip")
	}
	return &walletrpc.TreeState{Network: "main", Height: id.Height, OrchardTree: "01ab"}, nil
}

// StartLightwalletd serves l over TLS with a certificate for name.
func StartLightwalletd(ca *CA, name string, l *Lightwalletd) (addr string, stop func()) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{ca.Leaf(name)}})))
	walletrpc.RegisterCompactTxStreamerServer(srv, l)
	go srv.Serve(ln)
	return ln.Addr().String(), srv.Stop
}
