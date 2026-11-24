package main

// hasLocalRoute は cmd/tezzerd の dialLocalAddr と同じ経路解決トリック（UDP dial は
// パケットを送らず経路解決のみ）なので、環境非依存にループバック宛で検証する
// （実際の probeTargets は外部到達性がない環境では失敗するため、直接は使えない）。

import (
	"net"
	"slices"
	"testing"
)

func TestHasLocalRoute(t *testing.T) {
	t.Run("udp4 loopback has a route", func(t *testing.T) {
		if !hasLocalRoute("udp4", "127.0.0.1:80") {
			t.Error("hasLocalRoute(udp4, loopback) = false, want true")
		}
	})
	t.Run("udp6 loopback has a route", func(t *testing.T) {
		skipWithoutIPv6Loopback(t)
		if !hasLocalRoute("udp6", "[::1]:80") {
			t.Error("hasLocalRoute(udp6, loopback) = false, want true")
		}
	})
	t.Run("unresolvable target has no route", func(t *testing.T) {
		if hasLocalRoute("udp4", "this-host-does-not-exist.invalid:80") {
			t.Error("hasLocalRoute with unresolvable target = true, want false")
		}
	})
}

func TestRefreshCandidatesOnce(t *testing.T) {
	t.Run("no transport yet: does not panic, no-op", func(t *testing.T) {
		c := &Client{done: make(chan struct{})}
		c.refreshCandidatesOnce(&QUICInfo{Port: 4242, STUNAddrs: []string{"203.0.113.1:4242"}})
	})

	t.Run("resolveCandidates: client STUN unavailable falls back to server-provided candidates, not empty", func(t *testing.T) {
		c := &Client{done: make(chan struct{}), ipv4Only: true}

		// クライアント側の STUN 取得（hasLocalRoute のゲート、または実際の STUN
		// 問い合わせ自体）が失敗しても、quicInfo.STUNAddrs（サーバー申告の
		// グローバル候補）は buildQUICCandidateAddrs が無条件にフォールバックとして
		// 含めるので、候補が空にはならないはず（空なら refreshCandidatesOnce は
		// 既存の候補を維持してしまい、更新されない）。
		quicInfo := &QUICInfo{
			Port:       4242,
			STUNAddrs:  []string{"203.0.113.1:4242"},
			STUNServer: "127.0.0.1:1", // 実 STUN 到達性があっても即失敗する非到達アドレス
		}
		fresh, _ := c.resolveCandidates(quicInfo, true)
		if !slices.Contains(fresh, "203.0.113.1:4242") {
			t.Errorf("candidates = %v, want to include the server STUN addr", fresh)
		}
	})
}

func TestStartCandidateRefreshLoop_NoOpWithoutSTUN(t *testing.T) {
	// STUNAddrs が空（固定ポート運用等）なら family 冗長の余地がないので
	// goroutine を起動しない契約。起動していないことは直接観測できないが、
	// 少なくとも panic せず即座に戻ることを確認する。
	c := &Client{done: make(chan struct{})}
	c.startCandidateRefreshLoop(&QUICInfo{Port: 4242})
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
