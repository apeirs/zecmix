package mixnet

import "testing"

func TestEncodeAddrAndParse(t *testing.T) {
	if b, _ := EncodeAddr("zec.rocks"); b[0] != 3 || b[1] != 9 || string(b[2:]) != "zec.rocks" {
		t.Fatalf("domain: %v", b)
	}
	if b, _ := EncodeAddr("1.2.3.4"); len(b) != 5 || b[0] != 1 {
		t.Fatalf("ipv4: %v", b)
	}
	if b, _ := EncodeAddr("::1"); len(b) != 17 || b[0] != 4 {
		t.Fatalf("ipv6: %v", b)
	}
	if p, _ := ParseProxy("socks5h://127.0.0.1"); p != "127.0.0.1:1080" {
		t.Fatal(p)
	}
	for in, want := range map[string]string{"zec.rocks": "zec.rocks:443", "https://na.zec.rocks": "na.zec.rocks:443", "x.org:9067": "x.org:9067"} {
		if got, err := HostPort(in, 443); err != nil || got != want {
			t.Fatalf("HostPort(%q) = %q %v", in, got, err)
		}
	}
}
