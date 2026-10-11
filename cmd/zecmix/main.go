// Command zecmix: Zcash light-wallet traffic over the Nym mixnet.
//
//	zecmix nym setup                 start nym-socks5-client on a random active exit gateway
//	zecmix gateway                   local lightwalletd endpoint (127.0.0.1:9067) -> mixnet -> public server
//	zecmix info | tip | blocks | treestate | tx | send
//	zecmix ip                        which IP do servers see? (a Nym exit gateway)
package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/apeirs/zecmix/gateway"
	"github.com/apeirs/zecmix/lwd"
	"github.com/apeirs/zecmix/mixnet"
	"github.com/apeirs/zecmix/nym"
	"github.com/apeirs/zecmix/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const version = "0.1.0"

var start = time.Now()

func tracef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%7.2fs] "+format+"\n", append([]any{time.Since(start).Seconds()}, args...)...)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "zecmix:", err)
	os.Exit(1)
}

// common flags shared by network commands.
type common struct {
	proxy, dns, server string
	verbose, jsonOut   bool
	timeout            time.Duration
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.proxy, "proxy", envOr("ZECMIX_PROXY", "127.0.0.1:1080"), "nym-socks5-client SOCKS5 address ($ZECMIX_PROXY)")
	fs.StringVar(&c.dns, "dns", envOr("ZECMIX_DNS", mixnet.DNSDoH), "name resolution: doh (DoH over the mixnet, connect by IPv4) or exit (exit gateway resolves)")
	fs.StringVar(&c.server, "server", envOr("ZECMIX_SERVER", lwd.DefaultServer), "lightwalletd server host:port, TLS ($ZECMIX_SERVER)")
	fs.BoolVar(&c.verbose, "v", false, "trace every mixnet step to stderr")
	fs.BoolVar(&c.jsonOut, "json", false, "JSON output")
	fs.DurationVar(&c.timeout, "timeout", 3*time.Minute, "overall timeout for the command")
}

func (c *common) dialer() *mixnet.Dialer {
	proxy, err := mixnet.ParseProxy(c.proxy)
	if err != nil {
		die(err)
	}
	if c.dns != mixnet.DNSDoH && c.dns != mixnet.DNSExit {
		die(fmt.Errorf("--dns must be doh or exit"))
	}
	d := &mixnet.Dialer{Proxy: proxy, DNS: c.dns}
	if c.verbose {
		d.Trace = tracef
	}
	return d
}

func (c *common) client() (*grpc.ClientConn, walletrpc.CompactTxStreamerClient, *mixnet.Dialer) {
	d := c.dialer()
	if c.verbose {
		tracef("lwd  server %s over the Nym mixnet (socks5 %s, dns=%s); no clearnet fallback", c.server, d.Proxy, d.DNS)
	}
	cc, cl, err := lwd.Dial(d, c.server, lwd.Options{})
	if err != nil {
		die(err)
	}
	return cc, cl, d
}

func (c *common) ctx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	return ctx, func() { stop(); cancel() }
}

func (c *common) out(v any, human string) {
	if c.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}
	fmt.Println(human)
}

func grpcErr(what string, err error) error {
	if s, ok := status.FromError(err); ok {
		return fmt.Errorf("%s: %s: %s", what, s.Code(), s.Message())
	}
	return fmt.Errorf("%s: %w", what, err)
}

func usage() {
	fmt.Fprint(os.Stderr, `zecmix `+version+`: Zcash light-wallet traffic over the Nym mixnet

Mixnet client (official nym-socks5-client):
  zecmix nym setup [--binary PATH] [--port 1080]   init on a random active exit gateway, then start
  zecmix nym start | stop | status | exits

Local lightwalletd gateway for wallets (zingo-cli, zcash-devtool, ...):
  zecmix gateway [--listen 127.0.0.1:9067] [--tls] [--server zec.rocks:443]

Talk to lightwalletd directly over the mixnet:
  zecmix info                    GetLightdInfo
  zecmix tip                     GetLatestBlock
  zecmix blocks START [END]      GetBlockRange (streams compact blocks)
  zecmix treestate [HEIGHT]      GetTreeState / GetLatestTreeState
  zecmix tx TXID [--raw]         GetTransaction
  zecmix send HEX|FILE|-         SendTransaction (raw tx broadcast)
  zecmix ip                      which IP do servers see? (should be a Nym exit gateway)
  zecmix servers                 known public lightwalletd servers

Common flags: --server HOST:PORT --proxy 127.0.0.1:1080 --dns doh|exit -v --json --timeout 3m
There is no clearnet fallback: if the mixnet client is down, zecmix refuses to connect.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "nym":
		cmdNym(args)
	case "gateway":
		cmdGateway(args)
	case "info":
		cmdInfo(args)
	case "tip":
		cmdTip(args)
	case "blocks":
		cmdBlocks(args)
	case "treestate":
		cmdTreeState(args)
	case "tx":
		cmdTx(args)
	case "send":
		cmdSend(args)
	case "ip":
		cmdIP(args)
	case "servers":
		cmdServers(args)
	case "version", "--version":
		fmt.Println("zecmix", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "zecmix: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

// parse parses flags that may appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// ---------------------------------------------------------------- nym

func cmdNym(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("nym "+sub, flag.ExitOnError)
	binary := fs.String("binary", "", "path to nym-socks5-client ($ZECMIX_NYM_SOCKS5)")
	id := fs.String("id", nym.DefaultID, "client id (~/.nym/socks5-clients/ID)")
	port := fs.Int("port", nym.DefaultPort, "local SOCKS5 port")
	provider := fs.String("provider", "auto", "network requester address, or auto (random active exit gateway)")
	gw := fs.String("gateway", "", "entry gateway identity (default: chosen by the client)")
	noAnon := fs.Bool("no-anonymous", false, "disable reply SURBs (the exit then learns your mixnet address)")
	force := fs.Bool("force", false, "re-initialise (new identity, new exit)")
	minPerf := fs.Float64("min-performance", 0.95, "minimum exit performance for --provider auto")
	jsonOut := fs.Bool("json", false, "JSON output")
	parse(fs, args[1:])
	show := func(v any) {
		if *jsonOut {
			b, _ := json.MarshalIndent(v, "", "  ")
			fmt.Println(string(b))
			return
		}
		b, _ := json.Marshal(v)
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.UseNumber()
		var m map[string]any
		if d.Decode(&m) != nil {
			fmt.Println(string(b))
			return
		}
		for _, k := range []string{"id", "pid", "socks5", "provider", "anonymous_replies", "client_address", "gateway", "log"} {
			fmt.Printf("%-18s %v\n", k, m[k])
		}
	}
	switch sub {
	case "setup":
		bin, err := nym.FindBinary(*binary)
		if err != nil {
			die(err)
		}
		cfg := nym.ConfigPath(*id)
		if _, err := os.Stat(cfg); err == nil && !*force {
			fmt.Fprintf(os.Stderr, "client %q already initialised (%s); use --force to re-init\n", *id, cfg)
		} else {
			if *force {
				nym.Stop(*id)
				_ = os.RemoveAll(nym.Home() + "/socks5-clients/" + *id)
			}
			prov := *provider
			if prov == "auto" {
				ps, err := nym.ListExitProviders(nym.NymAPI, *minPerf)
				if err != nil {
					die(err)
				}
				p, err := nym.PickProvider(ps)
				if err != nil {
					die(err)
				}
				fmt.Fprintf(os.Stderr, "exit gateway: node %d (%s) perf=%.2f version=%s, picked at random from %d\n", p.NodeID, p.Country, p.Performance, p.Version, len(ps))
				prov = p.Address
			}
			if _, err := nym.Init(bin, *id, prov, *port, !*noAnon, *gw); err != nil {
				die(err)
			}
			fmt.Fprintf(os.Stderr, "initialised %q -> %s\n", *id, prov)
		}
		st, err := nym.Start(bin, *id, 3*time.Minute)
		if err != nil {
			die(err)
		}
		show(st)
	case "start":
		bin, err := nym.FindBinary(*binary)
		if err != nil {
			die(err)
		}
		st, err := nym.Start(bin, *id, 3*time.Minute)
		if err != nil {
			die(err)
		}
		show(st)
	case "stop":
		if nym.Stop(*id) {
			fmt.Println("stopped")
		} else {
			fmt.Println("not running")
		}
	case "status":
		st, err := nym.GetStatus(*id)
		if err != nil {
			die(err)
		}
		show(st)
	case "exits":
		ps, err := nym.ListExitProviders(nym.NymAPI, *minPerf)
		if err != nil {
			die(err)
		}
		if *jsonOut {
			show(ps)
			return
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NODE\tCC\tPERF\tVERSION\tNETWORK REQUESTER")
		for _, p := range ps {
			fmt.Fprintf(tw, "%d\t%s\t%.2f\t%s\t%s\n", p.NodeID, p.Country, p.Performance, p.Version, p.Address)
		}
		tw.Flush()
		fmt.Fprintf(os.Stderr, "%d active exit gateways with an exit-policy network requester\n", len(ps))
	default:
		die(fmt.Errorf("unknown nym subcommand %q", sub))
	}
}

// ---------------------------------------------------------------- lightwalletd calls

func cmdInfo(args []string) {
	var c common
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	c.register(fs)
	parse(fs, args)
	cc, cl, _ := c.client()
	defer cc.Close()
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	info, err := cl.GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		die(grpcErr("GetLightdInfo", err))
	}
	el := time.Since(t0).Seconds()
	human := fmt.Sprintf(`server            %s (via Nym mixnet, %.2fs)
chain             %s
block height      %d (estimated %d)
vendor            %s
version           %s (%s)
node              %s %s
consensus branch  %s
sapling active    %d
next upgrade      %s %d
protocol version  %s`, c.server, el, info.ChainName, info.BlockHeight, info.EstimatedHeight, info.Vendor, info.Version,
		info.GitCommit, info.ZcashdBuild, info.ZcashdSubversion, info.ConsensusBranchId, info.SaplingActivationHeight,
		orNone(info.UpgradeName), info.UpgradeHeight, orNone(info.LightwalletProtocolVersion))
	c.out(map[string]any{"server": c.server, "elapsed_s": el, "info": info}, human)
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdTip(args []string) {
	var c common
	fs := flag.NewFlagSet("tip", flag.ExitOnError)
	c.register(fs)
	parse(fs, args)
	cc, cl, _ := c.client()
	defer cc.Close()
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	b, err := cl.GetLatestBlock(ctx, &walletrpc.ChainSpec{})
	if err != nil {
		die(grpcErr("GetLatestBlock", err))
	}
	el := time.Since(t0).Seconds()
	c.out(map[string]any{"server": c.server, "height": b.Height, "hash": lwd.DisplayHash(b.Hash), "elapsed_s": el},
		fmt.Sprintf("%d  %s    (%s, via Nym mixnet, %.2fs)", b.Height, lwd.DisplayHash(b.Hash), c.server, el))
}

func parseHeight(s string, tip func() uint64) uint64 {
	if s == "tip" || s == "latest" {
		return tip()
	}
	if strings.HasPrefix(s, "tip-") {
		n, err := strconv.ParseUint(s[4:], 10, 64)
		if err != nil {
			die(fmt.Errorf("bad height %q", s))
		}
		return tip() - n
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		die(fmt.Errorf("bad height %q (use a number, tip, or tip-N)", s))
	}
	return n
}

func cmdBlocks(args []string) {
	var c common
	fs := flag.NewFlagSet("blocks", flag.ExitOnError)
	c.register(fs)
	quiet := fs.Bool("q", false, "summary only")
	pos := parse(fs, args)
	if len(pos) < 1 || len(pos) > 2 {
		die(errors.New("usage: zecmix blocks START [END]   (heights, or tip / tip-N)"))
	}
	cc, cl, _ := c.client()
	defer cc.Close()
	ctx, cancel := c.ctx()
	defer cancel()
	var tipH uint64
	tip := func() uint64 {
		if tipH == 0 {
			b, err := cl.GetLatestBlock(ctx, &walletrpc.ChainSpec{})
			if err != nil {
				die(grpcErr("GetLatestBlock", err))
			}
			tipH = b.Height
		}
		return tipH
	}
	from := parseHeight(pos[0], tip)
	to := from
	if len(pos) == 2 {
		to = parseHeight(pos[1], tip)
	}
	if to < from {
		die(errors.New("END must be >= START"))
	}
	t0 := time.Now()
	stream, err := cl.GetBlockRange(ctx, &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: from}, End: &walletrpc.BlockID{Height: to}})
	if err != nil {
		die(grpcErr("GetBlockRange", err))
	}
	type row struct {
		Height   uint64   `json:"height"`
		Hash     string   `json:"hash"`
		PrevHash string   `json:"prev_hash"`
		Time     string   `json:"time"`
		Txs      int      `json:"txs"`
		Sapling  int      `json:"sapling_outputs"`
		Spends   int      `json:"sapling_spends"`
		Orchard  int      `json:"orchard_actions"`
		Ironwood int      `json:"ironwood_actions"`
		TIn      int      `json:"transparent_inputs"`
		TOut     int      `json:"transparent_outputs"`
		OrchTree uint32   `json:"orchard_tree_size"`
		Bytes    int      `json:"bytes"`
		Txids    []string `json:"txids"`
	}
	var rows []row
	var total, outs, acts, iacts int
	var prev string
	if !*quiet && !c.jsonOut {
		fmt.Printf("%-8s %-64s %-19s %3s %5s %5s %6s %5s %5s %5s %6s\n", "HEIGHT", "HASH", "TIME (UTC)", "TXS", "T-IN", "T-OUT", "SAPOUT", "SAPSP", "ORCH", "IRONW", "BYTES")
	}
	for {
		blk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			die(grpcErr(fmt.Sprintf("GetBlockRange after %d blocks", len(rows)), err))
		}
		r := row{Height: blk.Height, Hash: lwd.DisplayHash(blk.Hash), PrevHash: lwd.DisplayHash(blk.PrevHash),
			Time: time.Unix(int64(blk.Time), 0).UTC().Format("2006-01-02 15:04:05"), Txs: len(blk.Vtx)}
		for _, tx := range blk.Vtx {
			r.Sapling += len(tx.Outputs)
			r.Spends += len(tx.Spends)
			r.Orchard += len(tx.Actions)
			r.Ironwood += len(tx.IronwoodActions)
			r.Txids = append(r.Txids, lwd.DisplayHash(tx.Txid))
			r.TIn += len(tx.Vin)
			r.TOut += len(tx.Vout)
		}
		if blk.ChainMetadata != nil {
			r.OrchTree = blk.ChainMetadata.OrchardCommitmentTreeSize
		}
		r.Bytes = protoSize(blk)
		if prev != "" && r.PrevHash != prev {
			die(fmt.Errorf("chain broken at %d: prev_hash %s != %s", r.Height, r.PrevHash, prev))
		}
		prev = r.Hash
		total += r.Bytes
		outs += r.Sapling
		acts += r.Orchard
		iacts += r.Ironwood
		rows = append(rows, r)
		if !*quiet && !c.jsonOut {
			fmt.Printf("%-8d %-64s %-19s %3d %5d %5d %6d %5d %5d %5d %6d\n", r.Height, r.Hash, r.Time, r.Txs, r.TIn, r.TOut, r.Sapling, r.Spends, r.Orchard, r.Ironwood, r.Bytes)
		}
	}
	el := time.Since(t0).Seconds()
	if want := int(to - from + 1); len(rows) != want {
		die(fmt.Errorf("server returned %d blocks, expected %d", len(rows), want))
	}
	summary := fmt.Sprintf("%d compact blocks %d..%d, %d bytes, %d sapling outputs, %d orchard + %d ironwood actions, prev-hash chain linked; %.2fs (%s, via Nym mixnet)",
		len(rows), from, to, total, outs, acts, iacts, el, c.server)
	if c.jsonOut {
		c.out(map[string]any{"server": c.server, "from": from, "to": to, "blocks": rows, "bytes": total, "elapsed_s": el}, "")
		return
	}
	fmt.Println(summary)
}

func cmdTreeState(args []string) {
	var c common
	fs := flag.NewFlagSet("treestate", flag.ExitOnError)
	c.register(fs)
	pos := parse(fs, args)
	cc, cl, _ := c.client()
	defer cc.Close()
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	var ts *walletrpc.TreeState
	var err error
	if len(pos) == 0 || pos[0] == "latest" || pos[0] == "tip" {
		ts, err = cl.GetLatestTreeState(ctx, &walletrpc.Empty{})
	} else {
		h := parseHeight(pos[0], func() uint64 { die(errors.New("use a number")); return 0 })
		ts, err = cl.GetTreeState(ctx, &walletrpc.BlockID{Height: h})
	}
	if err != nil {
		die(grpcErr("GetTreeState", err))
	}
	el := time.Since(t0).Seconds()
	c.out(map[string]any{"server": c.server, "treestate": ts, "elapsed_s": el},
		fmt.Sprintf("network       %s\nheight        %d\nhash          %s\ntime          %s\nsapling tree  %s (%d hex chars)\norchard tree  %s (%d hex chars)\n(%s, via Nym mixnet, %.2fs)",
			ts.Network, ts.Height, ts.Hash, time.Unix(int64(ts.Time), 0).UTC().Format(time.RFC3339),
			abbrev(ts.SaplingTree), len(ts.SaplingTree), abbrev(ts.OrchardTree), len(ts.OrchardTree), c.server, el))
}

func abbrev(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:12] + "…" + s[len(s)-12:]
}

func cmdTx(args []string) {
	var c common
	fs := flag.NewFlagSet("tx", flag.ExitOnError)
	c.register(fs)
	raw := fs.Bool("raw", false, "print only the raw transaction hex")
	pos := parse(fs, args)
	if len(pos) != 1 {
		die(errors.New("usage: zecmix tx TXID [--raw]"))
	}
	h, err := lwd.ParseDisplayHash(pos[0])
	if err != nil {
		die(err)
	}
	cc, cl, _ := c.client()
	defer cc.Close()
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	tx, err := cl.GetTransaction(ctx, &walletrpc.TxFilter{Hash: h})
	if err != nil {
		die(grpcErr("GetTransaction", err))
	}
	el := time.Since(t0).Seconds()
	if *raw {
		fmt.Println(hex.EncodeToString(tx.Data))
		return
	}
	where := fmt.Sprintf("mined at height %d", tx.Height)
	switch tx.Height {
	case 0:
		where = "in the mempool"
	case ^uint64(0):
		where = "mined on a fork (not main chain)"
	}
	_, hdr, _ := lwd.DecodeRawTx(hex.EncodeToString(tx.Data))
	c.out(map[string]any{"server": c.server, "txid": pos[0], "height": tx.Height, "hex": hex.EncodeToString(tx.Data), "elapsed_s": el},
		fmt.Sprintf("%s  %s, %s    (%s, via Nym mixnet, %.2fs)", pos[0], where, hdr, c.server, el))
}

func readTxArg(a string) string {
	if a == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			die(err)
		}
		return string(b)
	}
	if st, err := os.Stat(a); err == nil && !st.IsDir() {
		b, err := os.ReadFile(a)
		if err != nil {
			die(err)
		}
		return string(b)
	}
	return a
}

func cmdSend(args []string) {
	var c common
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	c.register(fs)
	pos := parse(fs, args)
	if len(pos) != 1 {
		die(errors.New("usage: zecmix send HEX|FILE|-"))
	}
	raw, hdr, err := lwd.DecodeRawTx(readTxArg(pos[0]))
	if err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "tx: %s\n", hdr)
	cc, cl, _ := c.client()
	defer cc.Close()
	ctx, cancel := c.ctx()
	defer cancel()
	t0 := time.Now()
	resp, err := cl.SendTransaction(ctx, &walletrpc.RawTransaction{Data: raw})
	el := time.Since(t0).Seconds()
	if err != nil {
		// Some servers (Zaino) report a rejection as a gRPC error status
		// rather than a SendResponse error code: the round trip still worked.
		if s, ok := status.FromError(err); ok && s.Code() != 14 && s.Code() != 4 && s.Code() != 1 {
			c.out(map[string]any{"server": c.server, "accepted": false, "grpc_code": s.Code().String(), "error": s.Message(), "elapsed_s": el},
				fmt.Sprintf("REJECTED  %s  grpc %s  %.2fs  %s    (via Nym mixnet)", c.server, s.Code(), el, s.Message()))
			os.Exit(3)
		}
		die(grpcErr("SendTransaction", err))
	}
	if resp.ErrorCode != 0 {
		c.out(map[string]any{"server": c.server, "accepted": false, "error_code": resp.ErrorCode, "error": resp.ErrorMessage, "elapsed_s": el},
			fmt.Sprintf("REJECTED  %s  code %d  %.2fs  %s    (via Nym mixnet)", c.server, resp.ErrorCode, el, resp.ErrorMessage))
		os.Exit(3)
	}
	c.out(map[string]any{"server": c.server, "accepted": true, "message": resp.ErrorMessage, "elapsed_s": el},
		fmt.Sprintf("ACCEPTED  %s  %.2fs  %s    (via Nym mixnet)", c.server, el, resp.ErrorMessage))
}

func cmdServers(args []string) {
	fs := flag.NewFlagSet("servers", flag.ExitOnError)
	parse(fs, args)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tNETWORK\tNOTE")
	for _, s := range lwd.KnownServers {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Addr, s.Network, s.Note)
	}
	tw.Flush()
	fmt.Println("\nAny lightwalletd/Zaino server works if its port is allowed by the Nym exit policy (443 and 9067 are).")
}

// ---------------------------------------------------------------- ip

func cmdIP(args []string) {
	var c common
	fs := flag.NewFlagSet("ip", flag.ExitOnError)
	c.register(fs)
	noLookup := fs.Bool("no-lookup", false, "don't look the IP up in the nym-api node list")
	parse(fs, args)
	d := c.dialer()
	ctx, cancel := c.ctx()
	defer cancel()
	const svc = "https://api.ipify.org"
	req, _ := newGet(ctx, svc)
	t0 := time.Now()
	resp, err := d.HTTPClient(c.timeout).Do(req)
	if err != nil {
		die(fmt.Errorf("GET %s over the mixnet: %w", svc, err))
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		die(fmt.Errorf("unexpected answer from %s: HTTP %d %q", svc, resp.StatusCode, ip))
	}
	el := time.Since(t0).Seconds()
	res := map[string]any{"service": svc, "seen_ip": ip, "elapsed_s": el}
	human := fmt.Sprintf("%s sees this request coming from %s (%.2fs over the mixnet)", svc, ip, el)
	if !*noLookup {
		// nym-api is queried directly, like nym-socks5-client does for topology.
		n, err := nym.NodeByIP(nym.NymAPI, ip)
		switch {
		case err != nil:
			human += fmt.Sprintf("\n(could not check the nym-api node list: %v)", err)
		case n == nil:
			human += "\nWARNING: that IP is not in the nym-api node list"
			res["nym_node"] = nil
		default:
			res["nym_node"] = map[string]any{"node_id": n.NodeID, "exit_nr": n.Description.Roles.ExitNR, "location": n.Description.Aux.Location}
			human += fmt.Sprintf("\n= Nym node %d (%s, exit gateway: %v) in the nym-api node list", n.NodeID, n.Description.Aux.Location, n.Description.Roles.ExitNR)
		}
	}
	c.out(res, human)
}

// ---------------------------------------------------------------- gateway

func cmdGateway(args []string) {
	var c common
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	c.register(fs)
	listen := fs.String("listen", "127.0.0.1:9067", "local listen address")
	useTLS := fs.Bool("tls", false, "serve TLS with a self-signed localhost certificate (for wallets that require https://)")
	allowRemote := fs.Bool("allow-remote", false, "allow a non-loopback listen address (anyone who can reach it can use your mixnet client)")
	noCheck := fs.Bool("no-check", false, "skip the startup self-test through the gateway")
	parse(fs, args)
	if !gateway.IsLoopback(*listen) && !*allowRemote {
		die(fmt.Errorf("refusing to listen on non-loopback %s without --allow-remote", *listen))
	}
	upstream, err := mixnet.HostPort(c.server, 443)
	if err != nil {
		die(err)
	}
	d := c.dialer()
	logf := func(format string, args ...any) { tracef(format, args...) }
	g := &gateway.Gateway{Upstream: upstream, Dialer: d, Log: logf}
	var tlsCfg *tls.Config
	scheme := "http"
	if *useTLS {
		cfg, path, fp, err := gateway.LocalCert(nym.StateDir())
		if err != nil {
			die(err)
		}
		tlsCfg = cfg
		scheme = "https"
		tracef("tls  self-signed cert %s (sha256 %s)", path, fp)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		die(err)
	}
	srv := g.Server(tlsCfg)
	go func() {
		var err error
		if tlsCfg != nil {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, errClosed) {
			die(err)
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if host == "127.0.0.1" && tlsCfg != nil {
		host = "localhost"
	}
	url := fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, port))
	tracef("gateway listening on %s -> socks5 %s -> Nym mixnet -> exit gateway -> %s (TLS)", url, d.Proxy, upstream)
	tracef("point your wallet's lightwalletd server at %s", url)
	if !*noCheck {
		go selfCheck(ln.Addr().String(), tlsCfg != nil, c.timeout)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	tracef("shutting down after %d calls", g.Calls())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// selfCheck calls GetLightdInfo through the local gateway (and therefore the mixnet).
func selfCheck(addr string, isTLS bool, timeout time.Duration) {
	opts := []grpc.DialOption{grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(lwd.MaxMsgSize))}
	if isTLS {
		opts = append(opts, grpc.WithTransportCredentials(insecureTLS()))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	cc, err := grpc.NewClient("passthrough:///"+addr, opts...)
	if err != nil {
		tracef("self-check: %v", err)
		return
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t0 := time.Now()
	info, err := walletrpc.NewCompactTxStreamerClient(cc).GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		tracef("self-check FAILED: %v (is nym-socks5-client running? `zecmix nym status`)", err)
		return
	}
	tracef("self-check ok: upstream %s %s, chain=%s height=%d (%.2fs through the gateway)", info.Vendor, info.Version, info.ChainName, info.BlockHeight, time.Since(t0).Seconds())
}
