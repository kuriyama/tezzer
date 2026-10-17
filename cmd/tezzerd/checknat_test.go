package main

import (
	"net"
	"testing"
)

// isLocalIP は -check-nat の「NAT なし」判定（STUN のマップドアドレスが自ホストの
// インターフェースに直接付いているか）に使われているが、テストが一本もなかった。
func TestIsLocalIP(t *testing.T) {
	if !isLocalIP(net.ParseIP("127.0.0.1")) {
		t.Error("127.0.0.1 (loopback) should always be local")
	}

	// RFC 5737 の TEST-NET-3（文書用に予約済み、絶対にどのホストにも実在しない）。
	if isLocalIP(net.ParseIP("203.0.113.1")) {
		t.Error("203.0.113.1 (TEST-NET-3) should never be local")
	}

	if isLocalIP(nil) {
		t.Error("nil IP should not be reported as local")
	}

	// 環境依存にせず、実際に付いているアドレスで確認する（127.0.0.1 だけだと
	// 「たまたまループバックしか見ていない」誤りを見逃しうる）。
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("net.InterfaceAddrs: %v", err)
	}
	found := false
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		found = true
		if !isLocalIP(ipnet.IP) {
			t.Errorf("isLocalIP(%s) = false, want true (it is one of net.InterfaceAddrs())", ipnet.IP)
		}
	}
	if !found {
		t.Fatal("net.InterfaceAddrs() returned no usable *net.IPNet, test is not meaningful")
	}
}
