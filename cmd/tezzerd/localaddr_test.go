package main

// dialLocalAddr/getLocalAddrs は同一LAN接続候補アドレス（クライアント側の
// buildUDPCandidateAddrs と対になるサーバー側のロジック、candidates_test.go 参照）
// の解決に使われているが、テストが一本もなかった。
//
// getLocalAddrs は実際には 8.8.8.8 / Google Public DNS の IPv6 アドレス宛に
// UDP dial する（パケットは送らず経路解決のみ）ため、外部到達性がない環境では
// 空を返す。dialLocalAddr 自体はループバック宛でも同じ経路（net.Dial → LocalAddr）
// を通るので、ループバックで直接テストする。

import (
	"net"
	"testing"
)

func TestDialLocalAddr(t *testing.T) {
	t.Run("udp4 loopback resolves to 127.0.0.1", func(t *testing.T) {
		got := dialLocalAddr("udp4", "127.0.0.1:80")
		if got != "127.0.0.1" {
			t.Errorf("dialLocalAddr(udp4, loopback) = %q, want 127.0.0.1", got)
		}
	})
	t.Run("udp6 loopback resolves to ::1", func(t *testing.T) {
		skipWithoutIPv6Loopback(t)
		got := dialLocalAddr("udp6", "[::1]:80")
		if got != "::1" {
			t.Errorf("dialLocalAddr(udp6, loopback) = %q, want ::1", got)
		}
	})
	t.Run("unresolvable target returns empty string", func(t *testing.T) {
		got := dialLocalAddr("udp4", "this-host-does-not-exist.invalid:80")
		if got != "" {
			t.Errorf("dialLocalAddr with unresolvable target = %q, want empty", got)
		}
	})
}

func TestGetLocalAddrs(t *testing.T) {
	t.Run("ipv4Only never returns an IPv6 address", func(t *testing.T) {
		addrs := getLocalAddrs(true)
		if len(addrs) > 1 {
			t.Fatalf("getLocalAddrs(ipv4Only=true) = %v, want at most 1 entry (no IPv6 dial attempted)", addrs)
		}
		for _, a := range addrs {
			ip := net.ParseIP(a)
			if ip == nil {
				t.Errorf("getLocalAddrs returned unparseable address %q", a)
				continue
			}
			if ip.To4() == nil {
				t.Errorf("getLocalAddrs(ipv4Only=true) returned non-IPv4 address %q", a)
			}
		}
	})
	t.Run("does not panic with both families", func(t *testing.T) {
		_ = getLocalAddrs(false)
	})
}

// skipWithoutIPv6Loopback は ::1 が使えない環境（IPv6 無効の Docker で動く CI ランナー等）で
// テストをスキップする。経路解決テストの前提（ループバックへの経路がある）が成り立たないため。
func skipWithoutIPv6Loopback(t *testing.T) {
	t.Helper()
	pc, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	pc.Close()
}
