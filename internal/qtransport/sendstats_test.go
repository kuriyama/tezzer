package qtransport

// backpressure 観測の土台: SendOutput が per-client の送信統計（bytes/最終送信時刻）を
// 記録し、ClientSendStats で取得できることを検証する。正常な localhost 経路では
// SlowWrites は増えない（＝詰まっていない）ことも確認する。

import (
	"context"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

func TestQUICTransport_ClientSendStats(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	cli, err := NewClient(k, addr, 7, "s1")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	// 出力を読み続ける（フロー制御を詰まらせない）。
	go func() {
		for range cli.Output() {
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for len(srv.ActiveClients()) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	const n = 5
	payload := []byte("0123456789") // 10 bytes
	for i := 0; i < n; i++ {
		if err := srv.SendOutput(uint64(i+1), payload, []transport.ClientID{{Session: "s1", Num: 7}}); err != nil {
			t.Fatalf("SendOutput: %v", err)
		}
	}

	// 統計に反映されるまで待つ。
	var st transport.ClientSendStat
	got := false
	for time.Now().Before(deadline) {
		for _, s := range srv.ClientSendStats() {
			if s.Client.Session == "s1" && s.Client.Num == 7 {
				st = s
				got = true
			}
		}
		if got && st.BytesSent >= uint64(n*len(payload)) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !got {
		t.Fatal("ClientSendStats did not include the client")
	}
	if st.BytesSent != uint64(n*len(payload)) {
		t.Fatalf("BytesSent=%d want %d", st.BytesSent, n*len(payload))
	}
	if st.LastSendUnix == 0 {
		t.Fatal("LastSendUnix not recorded")
	}
	if st.SlowWrites != 0 {
		t.Fatalf("SlowWrites=%d want 0 on fast localhost path", st.SlowWrites)
	}
}
