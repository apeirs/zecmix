package zecmix_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apeirs/zecmix/gateway"
	"github.com/apeirs/zecmix/internal/fakes"
	"github.com/apeirs/zecmix/lwd"
	"github.com/apeirs/zecmix/mixnet"
	"github.com/apeirs/zecmix/walletrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// world wires a fake mixnet: SOCKS proxy -> {DoH at 1.1.1.1, lightwalletd "lwd.test" at 10.0.0.7}.
type world struct {
	ca    *fakes.CA
	socks *fakes.SOCKS
	lwd   *fakes.Lightwalletd
	stop  func()
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ca := fakes.NewCA()
	doh := fakes.DoH(ca, "1.1.1.1", map[string][]string{"lwd.test": {"10.0.0.7"}})
	l := &fakes.Lightwalletd{Tip: 3513728}
	lwdAddr, stop := fakes.StartLightwalletd(ca, "lwd.test", l)
	s := fakes.NewSOCKS(map[string]string{
		"1.1.1.1:443":  doh,
		"10.0.0.7:443": lwdAddr,
		"lwd.test:443": lwdAddr,
	})
	t.Cleanup(func() { s.Close(); stop() })
	return &world{ca: ca, socks: s, lwd: l, stop: stop}
}

func (w *world) dialer(dns string) *mixnet.Dialer {
	return &mixnet.Dialer{Proxy: w.socks.Addr, DNS: dns, RootCAs: w.ca.Pool, Resolvers: []string{"1.1.1.1"}, Timeout: 5 * time.Second}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestCLIPathResolvesOverMixnetAndConnectsByIPv4(t *testing.T) {
	w := newWorld(t)
	cc, cl, err := lwd.Dial(w.dialer(mixnet.DNSDoH), "lwd.test:443", lwd.Options{RootCAs: w.ca.Pool})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	info, err := cl.GetLightdInfo(ctx(t), &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if info.ChainName != "main" || info.BlockHeight != 3513728 {
		t.Fatalf("unexpected info %+v", info)
	}
	seen := w.socks.Seen()
	// DoH to 1.1.1.1 by IP literal, then lightwalletd by the resolved IPv4: the
	// local resolver was never asked for "lwd.test".
	want := []string{"1.1.1.1:443(atyp 1)", "10.0.0.7:443(atyp 1)"}
	if len(seen) != 2 || seen[0].String() != want[0] || seen[1].String() != want[1] {
		t.Fatalf("SOCKS saw %v, want %v", seen, want)
	}
}

func TestExitDNSModePassesHostnameToExit(t *testing.T) {
	w := newWorld(t)
	cc, cl, err := lwd.Dial(w.dialer(mixnet.DNSExit), "lwd.test", lwd.Options{RootCAs: w.ca.Pool})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := cl.GetLatestBlock(ctx(t), &walletrpc.ChainSpec{}); err != nil {
		t.Fatal(err)
	}
	seen := w.socks.Seen()
	if len(seen) != 1 || seen[0].ATYP != 3 || seen[0].Host != "lwd.test" || seen[0].Port != 443 {
		t.Fatalf("SOCKS saw %v, want one domain-name CONNECT to lwd.test:443", seen)
	}
}

func TestNoClearnetFallbackWhenProxyDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	d := &mixnet.Dialer{Proxy: dead, DNS: mixnet.DNSDoH, Timeout: 2 * time.Second}
	_, err := d.DialContext(context.Background(), "tcp", "zec.rocks:443")
	if !errors.Is(err, mixnet.ErrProxyUnreachable) {
		t.Fatalf("want ErrProxyUnreachable, got %v", err)
	}
	cc, cl, err := lwd.Dial(d, "zec.rocks:443", lwd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cl.GetLightdInfo(c, &walletrpc.Empty{}); err == nil {
		t.Fatal("call succeeded without a mixnet proxy")
	}
}

func TestExitPolicyRefusalSurfaces(t *testing.T) {
	w := newWorld(t)
	_, err := w.dialer(mixnet.DNSExit).DialContext(context.Background(), "tcp", "elsewhere.test:25")
	var se *mixnet.SocksError
	if !errors.As(err, &se) || se.Code != 4 {
		t.Fatalf("want SOCKS reply 4, got %v", err)
	}
}

func TestBlockRangeStreamAndSendRejection(t *testing.T) {
	w := newWorld(t)
	cc, cl, err := lwd.Dial(w.dialer(mixnet.DNSDoH), "lwd.test:443", lwd.Options{RootCAs: w.ca.Pool})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	st, err := cl.GetBlockRange(ctx(t), &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: 100}, End: &walletrpc.BlockID{Height: 109}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		b, err := st.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Height != uint64(100+n) {
			t.Fatalf("block %d out of order", b.Height)
		}
		n++
	}
	if n != 10 {
		t.Fatalf("got %d blocks", n)
	}
	raw, _, err := lwd.DecodeRawTx("050000800a27a726" + strings.Repeat("00", 60))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cl.SendTransaction(ctx(t), &walletrpc.RawTransaction{Data: raw})
	if err != nil || resp.ErrorCode != -22 {
		t.Fatalf("want rejection -22, got %v %v", resp, err)
	}
}

func startGateway(t *testing.T, w *world, d *mixnet.Dialer) string {
	t.Helper()
	g := &gateway.Gateway{Upstream: "lwd.test:443", Dialer: d, RootCAs: w.ca.Pool, Log: t.Logf}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := g.Server(nil)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func TestGatewayProxiesWalletCallsOverMixnet(t *testing.T) {
	w := newWorld(t)
	addr := startGateway(t, w, w.dialer(mixnet.DNSDoH))
	// A "wallet" talking plaintext h2c gRPC to the local gateway.
	cc, cl, err := lwd.Dial(nil, "http://"+addr, lwd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	info, err := cl.GetLightdInfo(ctx(t), &walletrpc.Empty{})
	if err != nil || info.BlockHeight != 3513728 {
		t.Fatalf("info via gateway: %v %v", info, err)
	}
	st, err := cl.GetBlockRange(ctx(t), &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: 1}, End: &walletrpc.BlockID{Height: 50}})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		if _, err := st.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 50 {
		t.Fatalf("streamed %d blocks via gateway", n)
	}
	// Server-side gRPC errors (status in trailers) pass through intact.
	_, err = cl.GetTreeState(ctx(t), &walletrpc.BlockID{Height: 99999999})
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("want OutOfRange through gateway, got %v", err)
	}
	mds := w.lwd.Metadata()
	if len(mds) != 3 {
		t.Fatalf("upstream saw %d calls, want 3", len(mds))
	}
	for _, md := range mds {
		if a := md.Get(":authority"); len(a) != 1 || a[0] != "lwd.test" {
			t.Fatalf("upstream saw :authority %v, want lwd.test", a)
		}
		for _, h := range []string{"x-forwarded-for", "forwarded", "x-real-ip", "via"} {
			if v := md.Get(h); len(v) > 0 {
				t.Fatalf("gateway leaked %s: %v", h, v)
			}
		}
	}
	for _, r := range w.socks.Seen() {
		if r.Host != "1.1.1.1" && r.Host != "10.0.0.7" {
			t.Fatalf("unexpected SOCKS destination %v", r)
		}
	}
}

func TestGatewayReturnsUnavailableWithoutMixnet(t *testing.T) {
	w := newWorld(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	addr := startGateway(t, w, &mixnet.Dialer{Proxy: dead, Timeout: time.Second})
	cc, cl, err := lwd.Dial(nil, "http://"+addr, lwd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	_, err = cl.GetLightdInfo(ctx(t), &walletrpc.Empty{})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "mixnet") {
		t.Fatalf("want Unavailable mentioning the mixnet, got %v", err)
	}
	if len(w.lwd.Metadata()) != 0 {
		t.Fatal("upstream was reached without the mixnet")
	}
}

func TestPlaintextOnlyToLoopback(t *testing.T) {
	if _, _, err := lwd.Dial(nil, "http://zec.rocks:9067", lwd.Options{}); err == nil {
		t.Fatal("plaintext to a remote host must be refused")
	}
	if !gateway.IsLoopback("127.0.0.1:9067") || gateway.IsLoopback("0.0.0.0:9067") {
		t.Fatal("IsLoopback wrong")
	}
}
