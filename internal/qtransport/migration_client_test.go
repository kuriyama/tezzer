package qtransport

// qtransport クライアントの能動 migration（ローミング）検証。
// 入力をエコーするサーバに接続し、migrate() 後も同一ストリームで往復が継続することを確認する。

import (
	"context"
	"crypto/rand"
	"github.com/kuriyama/tezzer/internal/transport"
	"net"
	"testing"
	"time"
)

func TestQUICTransport_ClientMigration(t *testing.T) {
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

	// 入力をそのまま出力へエコー（offset 採番）。
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

	addr := srv.(interface{ Addr() net.Addr }).Addr().String()
	cli, err := NewClient(k, addr, 99, "")
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
		if err := cli.SendInput([]byte(msg)); err != nil {
			t.Fatalf("SendInput %q: %v", msg, err)
		}
		select {
		case chunk := <-cli.Output():
			out := chunk.Data
			if string(out) != msg {
				t.Fatalf("echo mismatch: got %q want %q", out, msg)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no echo for %q", msg)
		}
	}

	roundTrip("before-migration")

	// 能動 migration（新ローカル socket へ）。AddPath/Probe/Switch の機構自体は
	// spike B（TestSpikeB_Migration_StreamSurvives）で厳密に実証済み。ここでは
	// クライアント実装の migrate() が成功し、同一ストリームで往復が継続することを検証する。
	// 注: conn.LocalAddr() は migration 中に quic-go 内部で更新されるため、ここでは読まない
	// （-race 競合になる。LocalAddr 変化の検証は spike B が担う）。
	if err := qc.migrate("test"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 同一ストリームで往復が継続すること（migration がストリーム状態を保持）。
	roundTrip("after-migration-1")
	roundTrip("after-migration-2")
}
