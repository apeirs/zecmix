package zecmix_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/apeirs/zecmix/lwd"
	"github.com/apeirs/zecmix/mixnet"
	"github.com/apeirs/zecmix/walletrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The test binary doubles as a fake nymdial: run with ZECMIX_FAKE_NYMDIAL=1 it
// behaves like `nymdial [opts] connect HOST PORT --events`. FAKE_NYMDIAL_ROUTES
// maps "host:port" to a local address, FAKE_NYMDIAL_LOG gets one JSON line per
// run, FAKE_NYMDIAL_MODE is ok | error | hang.
func TestMain(m *testing.M) {
	if os.Getenv("ZECMIX_FAKE_NYMDIAL") == "1" {
		os.Exit(fakeNymdial(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeRun struct {
	PID  int      `json:"pid"`
	Argv []string `json:"argv"`
}

func fakeNymdial(args []string) int {
	if p := os.Getenv("FAKE_NYMDIAL_LOG"); p != "" {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		b, _ := json.Marshal(fakeRun{os.Getpid(), args})
		f.Write(append(b, '\n'))
		f.Close()
	}
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(os.Stderr, string(b))
	}
	i := slices.Index(args, "connect")
	host, port := args[i+1], args[i+2]
	node := 1000 + rand.IntN(9000)
	label := fmt.Sprintf("node %d (FR)", node)
	fmt.Fprintf(os.Stderr, "  exit %s up after 0.01s (incl. health check)\n", label)
	switch os.Getenv("FAKE_NYMDIAL_MODE") {
	case "error":
		emit(map[string]any{"event": "error", "step": "exit", "message": "no working exit after 1 attempts (last: health check failed)"})
		return 1
	case "hang":
		time.Sleep(time.Hour)
		return 1
	}
	var routes map[string]string
	_ = json.Unmarshal([]byte(os.Getenv("FAKE_NYMDIAL_ROUTES")), &routes)
	target, ok := routes[net.JoinHostPort(host, port)]
	if !ok {
		emit(map[string]any{"event": "error", "step": "dns", "message": "DNS over the mixnet for " + host + ": NXDOMAIN"})
		return 1
	}
	up, err := net.Dial("tcp", target)
	if err != nil {
		emit(map[string]any{"event": "error", "step": "tcp", "message": err.Error()})
		return 1
	}
	emit(map[string]any{"event": "connected", "host": host, "tunnel_s": 0.01, "connect_s": 0.01,
		"exit": map[string]any{"node_id": node, "country": "FR", "ips": []string{fmt.Sprintf("192.0.2.%d", node%250+1)}, "label": label}})
	c, err := net.FileConn(os.Stdin)
	if err != nil {
		return 1
	}
	go func() {
		io.Copy(up, c)
		up.(*net.TCPConn).CloseWrite()
	}()
	io.Copy(c, up) // until the server closes
	return 0
}

type nymWorld struct {
	*world
	log string
}

func newNymWorld(t *testing.T, mode string) *nymWorld {
	w := &nymWorld{world: newWorld(t), log: filepath.Join(t.TempDir(), "runs.jsonl")}
	t.Setenv("ZECMIX_FAKE_NYMDIAL", "1")
	t.Setenv("FAKE_NYMDIAL_LOG", w.log)
	t.Setenv("FAKE_NYMDIAL_MODE", mode)
	routes, _ := json.Marshal(map[string]string{"lwd.test:443": w.socksRoute("lwd.test:443")})
	t.Setenv("FAKE_NYMDIAL_ROUTES", string(routes))
	return w
}

// socksRoute reuses the world's lightwalletd address (the fake SOCKS proxy knows it).
func (w *nymWorld) socksRoute(hp string) string { return w.socks.Routes[hp] }

func (w *nymWorld) dialer(args ...string) *mixnet.Dialer {
	return &mixnet.Dialer{Nymdial: &mixnet.Nymdial{Binary: os.Args[0], Args: args, SetupTimeout: 10 * time.Second}}
}

func (w *nymWorld) runs(t *testing.T) []fakeRun {
	f, err := os.Open(w.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []fakeRun
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r fakeRun
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func gone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestNymdialCarriesLightwalletdWithoutLocalDNS(t *testing.T) {
	w := newNymWorld(t, "ok")
	d := w.dialer()
	cc, cl, err := lwd.Dial(d, "lwd.test:443", lwd.Options{RootCAs: w.ca.Pool})
	if err != nil {
		t.Fatal(err)
	}
	info, err := cl.GetLightdInfo(ctx(t), &walletrpc.Empty{})
	if err != nil || info.BlockHeight != 3513728 {
		t.Fatalf("info: %v %v", info, err)
	}
	st, err := cl.GetBlockRange(ctx(t), &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: 100}, End: &walletrpc.BlockID{Height: 149}})
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
		t.Fatalf("streamed %d blocks", n)
	}
	raw, _, _ := lwd.DecodeRawTx("050000800a27a726" + strings.Repeat("00", 60))
	if resp, err := cl.SendTransaction(ctx(t), &walletrpc.RawTransaction{Data: raw}); err != nil || resp.ErrorCode != -22 {
		t.Fatalf("want rejection -22, got %v %v", resp, err)
	}
	runs := w.runs(t)
	if len(runs) != 1 {
		t.Fatalf("want one nymdial process for one gRPC connection, got %d", len(runs))
	}
	if got := strings.Join(runs[0].Argv, " "); got != "connect lwd.test 443 --events" {
		t.Fatalf("nymdial argv %q: the hostname must go to nymdial unresolved", got)
	}
	if seen := w.socks.Seen(); len(seen) != 0 {
		t.Fatalf("nothing may use the SOCKS path (or its DoH) on the nymdial transport, saw %v", seen)
	}
	ex := d.Exits()
	if len(ex) != 1 || ex[0].Country != "FR" || ex[0].NodeID == 0 || !strings.HasPrefix(ex[0].IPs[0], "192.0.2.") {
		t.Fatalf("exit not recorded: %+v", ex)
	}
	cc.Close()
	if !gone(runs[0].PID, 8*time.Second) {
		t.Fatal("nymdial child still running after the connection closed")
	}
}

func TestNymdialGatewayServesAWalletOverOneProcess(t *testing.T) {
	w := newNymWorld(t, "ok")
	addr := startGateway(t, w.world, w.dialer())
	cc, cl, err := lwd.Dial(nil, "http://"+addr, lwd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := cl.GetLightdInfo(ctx(t), &walletrpc.Empty{}); err != nil {
		t.Fatal(err)
	}
	tip, err := cl.GetLatestBlock(ctx(t), &walletrpc.ChainSpec{})
	if err != nil || tip.Height != 3513728 {
		t.Fatalf("tip %v %v", tip, err)
	}
	st, err := cl.GetBlockRange(ctx(t), &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: tip.Height - 9}, End: &walletrpc.BlockID{Height: tip.Height}})
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
	if n != 10 {
		t.Fatalf("streamed %d blocks", n)
	}
	if _, err := cl.GetTreeState(ctx(t), &walletrpc.BlockID{Height: 99999999}); status.Code(err) != codes.OutOfRange {
		t.Fatalf("want OutOfRange through the gateway, got %v", err)
	}
	if runs := w.runs(t); len(runs) != 1 {
		t.Fatalf("want the gateway's one upstream connection = one nymdial process, got %d", len(runs))
	}
	for _, md := range w.lwd.Metadata() {
		for _, h := range []string{"x-forwarded-for", "forwarded", "x-real-ip", "via"} {
			if v := md.Get(h); len(v) > 0 {
				t.Fatalf("gateway leaked %s: %v", h, v)
			}
		}
	}
}

func TestNymdialMissingNeverGoesDirect(t *testing.T) {
	w := newNymWorld(t, "ok")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ZECMIX_NYMDIAL", "")
	if _, err := mixnet.FindNymdial(""); !errors.Is(err, mixnet.ErrNymdialMissing) {
		t.Fatalf("want ErrNymdialMissing, got %v", err)
	}
	if _, err := mixnet.FindNymdial("/nonexistent/nymdial"); !errors.Is(err, mixnet.ErrNymdialMissing) {
		t.Fatalf("want ErrNymdialMissing for a bad explicit path, got %v", err)
	}
	d := &mixnet.Dialer{Nymdial: &mixnet.Nymdial{Binary: "/nonexistent/nymdial"}}
	if _, err := d.DialContext(context.Background(), "tcp", "lwd.test:443"); !errors.Is(err, mixnet.ErrNymdialMissing) {
		t.Fatalf("want ErrNymdialMissing, got %v", err)
	}
	addr := startGateway(t, w.world, d)
	cc, cl, err := lwd.Dial(nil, "http://"+addr, lwd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := cl.GetLightdInfo(ctx(t), &walletrpc.Empty{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
	if len(w.lwd.Metadata()) != 0 || len(w.socks.Seen()) != 0 {
		t.Fatal("upstream was reached without nymdial")
	}
}

func TestNymdialSetupErrorNamesTheStep(t *testing.T) {
	w := newNymWorld(t, "error")
	_, err := w.dialer().DialContext(ctx(t), "tcp", "lwd.test:443")
	var ne *mixnet.NymdialError
	if !errors.As(err, &ne) || ne.Step != "exit" || !strings.Contains(err.Error(), "no working exit") {
		t.Fatalf("want a NymdialError at step exit, got %v", err)
	}
	t.Setenv("FAKE_NYMDIAL_MODE", "ok")
	_, err = w.dialer().DialContext(ctx(t), "tcp", "unknown.test:443")
	if !errors.As(err, &ne) || ne.Step != "dns" {
		t.Fatalf("want a NymdialError at step dns, got %v", err)
	}
}

func TestNymdialSetupTimeoutStopsTheChild(t *testing.T) {
	w := newNymWorld(t, "hang")
	d := w.dialer()
	d.Nymdial.SetupTimeout = time.Second
	t0 := time.Now()
	_, err := d.DialContext(ctx(t), "tcp", "lwd.test:443")
	var ne *mixnet.NymdialError
	if !errors.As(err, &ne) || ne.Step != "timeout" {
		t.Fatalf("want a timeout, got %v", err)
	}
	if time.Since(t0) > 8*time.Second {
		t.Fatalf("timeout took %s", time.Since(t0))
	}
	runs := w.runs(t)
	if len(runs) != 1 || !gone(runs[0].PID, 8*time.Second) {
		t.Fatal("hung nymdial child was not stopped")
	}
}

func TestNymdialContextCancelStopsSetup(t *testing.T) {
	w := newNymWorld(t, "hang")
	c, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := w.dialer().DialContext(c, "tcp", "lwd.test:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context deadline, got %v", err)
	}
}

func TestNymdialExitOptionsComeBeforeConnect(t *testing.T) {
	w := newNymWorld(t, "ok")
	c, err := w.dialer("--country", "DE", "--exit", "2205").DialContext(ctx(t), "tcp", "lwd.test:443")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if got := strings.Join(w.runs(t)[0].Argv, " "); got != "--country DE --exit 2205 connect lwd.test 443 --events" {
		t.Fatalf("argv %q", got)
	}
}
