package qtransport

// stall watchdog のテスト: 出力を読まないクライアントがいると SendOutput が
// フロー制御でブロックする（= PTY reader が止まる実挙動の再現）。watchdog が
// warning 水位超えを検知して、同セッションの「他の」クライアントへステータス
// 通知すること、ClientSendStats に stall 統計が載ることを検証する。

import (
	"context"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/quic-go/quic-go"
)

func TestQUICTransport_StallWarnsOtherClients(t *testing.T) {
	// 水位をテスト用に短縮（watchdog は起動時に読むので Start 前に設定する）。
	oldWarn, oldRepeat, oldTick := stallWarnThreshold, stallWarnRepeat, stallCheckInterval
	stallWarnThreshold, stallWarnRepeat, stallCheckInterval = 300*time.Millisecond, time.Second, 50*time.Millisecond
	t.Cleanup(func() {
		stallWarnThreshold, stallWarnRepeat, stallCheckInterval = oldWarn, oldRepeat, oldTick
	})

	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	// 健全なクライアント（Num 7）: 出力を読み続け、status 通知を受け取る側。
	healthy, err := NewClient(k, addr, 7, "s1")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer healthy.Close()
	statusCh := make(chan string, 16)
	healthy.OnStatusMessage(func(msg string) {
		select {
		case statusCh <- msg:
		default:
		}
	})
	if err := healthy.Start(ctx); err != nil {
		t.Fatalf("healthy Start: %v", err)
	}
	go func() {
		for range healthy.Output() {
		}
	}()

	// 詰まるクライアント（Num 8）: Hello だけ送って以後何も読まない raw QUIC 接続。
	// 出力ストリームの受信ウィンドウ（quic-go デフォルト初期 512KB）を吸い切ると
	// サーバ側の writeOutputFrame がブロックする（スリープ中クライアントの再現）。
	tlsConf, err := ClientTLS(k)
	if err != nil {
		t.Fatalf("ClientTLS: %v", err)
	}
	stalledConn, err := quic.DialAddr(ctx, addr, tlsConf, quicConfig())
	if err != nil {
		t.Fatalf("DialAddr (stalled): %v", err)
	}
	defer stalledConn.CloseWithError(0, "test done")
	ctrl, err := stalledConn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	if err := writeFrame(ctrl, &ctrlMsg{Type: ctrlHello, ClientID: 8, SessionID: "s1"}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(srv.ActiveClients()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(srv.ActiveClients()) < 2 {
		t.Fatalf("clients did not register: %v", srv.ActiveClients())
	}

	// PTY reader 相当の送信ループ: Num 8 への Write がブロックするとループごと止まる。
	go func() {
		payload := make([]byte, 64*1024)
		targets := []transport.ClientID{{Session: "s1", Num: 7}, {Session: "s1", Num: 8}}
		for i := uint64(1); ctx.Err() == nil; i++ {
			_ = srv.SendOutput(i, payload, targets)
		}
	}()

	// 健全なクライアントに stall 通知が届くこと。
	var status string
	select {
	case status = <-statusCh:
	case <-time.After(10 * time.Second):
		t.Fatal("healthy client did not receive stall status")
	}
	if !strings.Contains(status, "stalled") || !strings.Contains(status, "client 8") {
		t.Fatalf("unexpected status message: %q", status)
	}

	// 統計: 詰まっているクライアントに stall が計上され、健全な側には付かないこと。
	var stalledStat, healthyStat transport.ClientSendStat
	for _, st := range srv.ClientSendStats() {
		switch st.Client.Num {
		case 8:
			stalledStat = st
		case 7:
			healthyStat = st
		}
	}
	if stalledStat.StallEpisodes < 1 {
		t.Errorf("StallEpisodes = %d, want >= 1", stalledStat.StallEpisodes)
	}
	if stalledStat.CurrentStallMs < uint64(stallWarnThreshold.Milliseconds()) {
		t.Errorf("CurrentStallMs = %d, want >= %d", stalledStat.CurrentStallMs, stallWarnThreshold.Milliseconds())
	}
	if healthyStat.StallEpisodes != 0 || healthyStat.CurrentStallMs != 0 {
		t.Errorf("healthy client has stall stats: episodes=%d current=%dms",
			healthyStat.StallEpisodes, healthyStat.CurrentStallMs)
	}
}
