package zecmix_test

import (
	"io"
	"os"
	"testing"

	"github.com/apeirs/zecmix/lwd"
	"github.com/apeirs/zecmix/mixnet"
	"github.com/apeirs/zecmix/walletrpc"
)

// Opt-in: ZECMIX_LIVE=1 go test -run Live -v .   (needs nym-socks5-client on $ZECMIX_PROXY or 127.0.0.1:1080)
func TestLiveMainnetOverMixnet(t *testing.T) {
	if os.Getenv("ZECMIX_LIVE") == "" {
		t.Skip("set ZECMIX_LIVE=1 to talk to mainnet lightwalletd through the real Nym mixnet")
	}
	proxy := os.Getenv("ZECMIX_PROXY")
	if proxy == "" {
		proxy = "127.0.0.1:1080"
	}
	server := os.Getenv("ZECMIX_SERVER")
	if server == "" {
		server = lwd.DefaultServer
	}
	d := &mixnet.Dialer{Proxy: proxy, DNS: mixnet.DNSDoH, Trace: t.Logf}
	cc, cl, err := lwd.Dial(d, server, lwd.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	c := ctx(t)
	info, err := cl.GetLightdInfo(c, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if info.ChainName != "main" || info.BlockHeight < 3_500_000 {
		t.Fatalf("unexpected info %+v", info)
	}
	tip, err := cl.GetLatestBlock(c, &walletrpc.ChainSpec{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := cl.GetBlockRange(c, &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: tip.Height - 4}, End: &walletrpc.BlockID{Height: tip.Height}})
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
	if n != 5 {
		t.Fatalf("got %d blocks", n)
	}
	raw, _, _ := lwd.DecodeRawTx("050000800a27a726" + "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000")
	resp, err := cl.SendTransaction(c, &walletrpc.RawTransaction{Data: raw})
	if err == nil && resp.ErrorCode == 0 {
		t.Fatal("invalid tx was accepted?!")
	}
	t.Logf("tip %d, 5 blocks streamed, invalid tx rejected: %v %v", tip.Height, resp, err)
}
