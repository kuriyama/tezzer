package main

// hasLocalRoute は cmd/tezzerd の dialLocalAddr と同じ経路解決トリック（UDP dial は
// パケットを送らず経路解決のみ）なので、環境非依存にループバック宛で検証する
// （実際の probeTargets は外部到達性がない環境では失敗するため、直接は使えない）。

import (
	"context"
	"net"
	"testing"

	"github.com/kuriyama/tezzer/internal/transport"
)

// fakeClientTransport は transport.ClientTransport の最小スタブ。
// refreshCandidatesOnce が SetRoamingCandidates をどう呼ぶかだけを見る。
type fakeClientTransport struct {
	roamingCandidates []string
}

func (f *fakeClientTransport) Start(context.Context) error          { return nil }
func (f *fakeClientTransport) Close() error                         { return nil }
func (f *fakeClientTransport) SendInput([]byte) error               { return nil }
func (f *fakeClientTransport) SendResize(int, int) error            { return nil }
func (f *fakeClientTransport) Output() <-chan transport.OutputChunk { return nil }
func (f *fakeClientTransport) State() transport.ConnectionState     { return transport.StateConnected }
func (f *fakeClientTransport) OnStateChange(func(transport.ConnectionState, transport.ConnectionState, string)) {
}
func (f *fakeClientTransport) OnStatusMessage(func(string))              {}
func (f *fakeClientTransport) OnLogMessage(func(string))                 {}
func (f *fakeClientTransport) OnServerMeta(func(string, string, []byte)) {}
func (f *fakeClientTransport) OnSessionNotFound(func(string, int))       {}
func (f *fakeClientTransport) Stats() transport.Stats                    { return transport.Stats{} }
func (f *fakeClientTransport) SetRoamingCandidates(addrs []string)       { f.roamingCandidates = addrs }
func (f *fakeClientTransport) SetCandidatesRefresher(func() []string)    {}

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

	t.Run("client STUN unavailable falls back to server-provided candidates, not empty", func(t *testing.T) {
		c := &Client{done: make(chan struct{}), ipv4Only: true}
		ft := &fakeClientTransport{}
		c.setTransport(ft)

		// クライアント側の STUN 取得（hasLocalRoute のゲート、または実際の STUN
		// 問い合わせ自体）が失敗しても、quicInfo.STUNAddrs（サーバー申告の
		// グローバル候補）は buildQUICCandidateAddrs が無条件にフォールバックとして
		// 含めるので、候補が空にはならないはず。
		quicInfo := &QUICInfo{
			Port:       4242,
			STUNAddrs:  []string{"203.0.113.1:4242"},
			STUNServer: "127.0.0.1:1", // 実 STUN 到達性があっても即失敗する非到達アドレス
		}
		c.refreshCandidatesOnce(quicInfo)

		if len(ft.roamingCandidates) == 0 {
			t.Fatal("SetRoamingCandidates was not called with any candidates")
		}
		found := false
		for _, a := range ft.roamingCandidates {
			if a == "203.0.113.1:4242" {
				found = true
			}
		}
		if !found {
			t.Errorf("roaming candidates = %v, want to include the server STUN addr", ft.roamingCandidates)
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
