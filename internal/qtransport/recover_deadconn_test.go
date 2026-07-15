package qtransport

// 接続が死んだ（idle timeout 相当）状態を強制し、recover() が connDead を検出して
// reconnect にフォールバックし、往復が回復することを検証する。
// （スリープ復帰時の「Recoveries=0 のまま固まる」バグの回帰防止。実機の wall-clock
// サスペンド検出自体は単体テスト困難なので、ここでは conn 死→recover→reconnect 経路を担保。）

import (
	"context"
	"crypto/rand"
	"github.com/kuriyama/tezzer/internal/transport"
	"net"
	"testing"
	"time"
)

func TestQUICTransport_RecoverOnDeadConn(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	var off uint64
	go func() {
		for {
			in, ok := <-srv.Input()
			if !ok {
				return
			}
			off++
			_ = srv.SendOutput(off, in.Data, []transport.ClientID{in.Client})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	cli, err := NewClient(k, addr, 7, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	qc := cli.(*quicClient)

	roundTrip := func(msg string) {
		t.Helper()
		_ = cli.SendInput([]byte(msg))
		select {
		case chunk := <-cli.Output():
			out := chunk.Data
			if string(out) != msg {
				t.Fatalf("echo got %q want %q", out, msg)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no echo for %q", msg)
		}
	}

	roundTrip("before")

	// 接続を強制終了（idle timeout で死んだ状態を模擬）。
	qc.conn.CloseWithError(0, "simulated idle death")
	deadline := time.Now().Add(3 * time.Second)
	for !qc.connDead() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !qc.connDead() {
		t.Fatal("conn did not become dead after CloseWithError")
	}

	// recover() は connDead を見て reconnect にフォールバックするはず。
	qc.recover("test dead conn")

	if st := cli.Stats(); st.RecoveryCount != 1 {
		t.Fatalf("RecoveryCount=%d want 1 after recover", st.RecoveryCount)
	}
	if got := cli.State().String(); got != "Connected" {
		t.Fatalf("state after recover = %s want Connected", got)
	}

	// 新しい接続で往復が回復すること。
	roundTrip("after-recover")
}
