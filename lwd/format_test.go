package lwd

import (
	"strings"
	"testing"
)

func TestDisplayHashRoundTrip(t *testing.T) {
	txid := "712f81c604b20db3b66f72d0f688e1e292e6abead4ba70672f69b14f2ad35498"
	b, err := ParseDisplayHash(txid)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 0x98 || b[31] != 0x71 {
		t.Fatalf("not byte-reversed: %x", b)
	}
	if DisplayHash(b) != txid {
		t.Fatal("round trip failed")
	}
	if _, err := ParseDisplayHash("abcd"); err == nil {
		t.Fatal("short txid accepted")
	}
}

func TestDecodeRawTx(t *testing.T) {
	// Header of mainnet tx 712f81c6… (block 3,513,720, fetched over the mixnet): v6, NU6.3 branch.
	h6 := "06000080" + "98b684d8" + "5b16a537" + strings.Repeat("00", 20)
	_, hdr, err := DecodeRawTx(h6)
	if err != nil || hdr.Version != 6 || !hdr.Overwintered || hdr.ConsensusBrID != 0x37a5165b {
		t.Fatalf("v6 header: %+v %v", hdr, err)
	}
	_, hdr, err = DecodeRawTx("05000080 0a27a726\n" + strings.Repeat("00", 60))
	if err != nil || hdr.Version != 5 || hdr.Size != 68 {
		t.Fatalf("v5 header with whitespace: %+v %v", hdr, err)
	}
	for _, bad := range []string{"zz", "0400", "0500008011111111" + strings.Repeat("00", 20), "ff000000" + strings.Repeat("00", 20)} {
		if _, _, err := DecodeRawTx(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
