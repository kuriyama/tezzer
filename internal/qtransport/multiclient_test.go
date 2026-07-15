package qtransport

// 共有ポートモードの土台: 1 つのサーバ（1 ソケット）に複数クライアントが
// connection ID 多重化で相乗りし、SendOutput が clientID 単位で正しく分離される
// （他クライアントへ漏れない）ことを検証する。

import (
	"context"
	"crypto/rand"
	"github.com/kuriyama/tezzer/internal/transport"
	"net"
	"testing"
	"time"
)

func TestQUICTransport_MultiClientIsolation(t *testing.T) {
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

	connect := func(id uint16) ClientTransportWithClose {
		c, err := NewClient(k, addr, id, "")
		if err != nil {
			t.Fatalf("NewClient(%d): %v", id, err)
		}
		if err := c.Start(ctx); err != nil {
			t.Fatalf("client %d Start: %v", id, err)
		}
		return c
	}

	c1 := connect(101)
	defer c1.Close()
	c2 := connect(202)
	defer c2.Close()

	// 両クライアントが接続するまで待つ（同一サーバ＝1 ソケットに 2 接続）。
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.ActiveClients()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(srv.ActiveClients()); got != 2 {
		t.Fatalf("expected 2 active clients on one socket, got %d", got)
	}

	// clientID 101 だけに送る → c1 のみ受信、c2 は受信しないこと。
	if err := srv.SendOutput(1, []byte("for-101"), []transport.ClientID{{Num: 101}}); err != nil {
		t.Fatalf("SendOutput 101: %v", err)
	}
	select {
	case chunk := <-c1.Output():
		out := chunk.Data
		if string(out) != "for-101" {
			t.Fatalf("c1 got %q want for-101", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("c1 did not receive its output")
	}
	select {
	case chunk := <-c2.Output():
		out := chunk.Data
		t.Fatalf("c2 received cross-talk: %q", out)
	case <-time.After(500 * time.Millisecond):
		// 期待どおり c2 には来ない
	}

	// 逆向きも確認。
	if err := srv.SendOutput(1, []byte("for-202"), []transport.ClientID{{Num: 202}}); err != nil {
		t.Fatalf("SendOutput 202: %v", err)
	}
	select {
	case chunk := <-c2.Output():
		out := chunk.Data
		if string(out) != "for-202" {
			t.Fatalf("c2 got %q want for-202", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("c2 did not receive its output")
	}
}

// ClientTransportWithClose は test 内で Close を呼ぶための最小インターフェース。
type ClientTransportWithClose interface {
	Start(context.Context) error
	Output() <-chan transport.OutputChunk
	Close() error
}

// TestQUICTransport_SameNumDifferentSession は A（(SessionID, ClientID) routing）の回帰防止。
// 別セッションのクライアントが同じ Num（クライアント自己採番の uint16）を選んでも、
// 出力が混線せず各セッションへ正しく分離されることを検証する。
func TestQUICTransport_SameNumDifferentSession(t *testing.T) {
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

	// 同じ Num=7、別セッション "A" / "B"。
	ca, err := NewClient(k, addr, 7, "A")
	if err != nil {
		t.Fatalf("NewClient A: %v", err)
	}
	defer ca.Close()
	if err := ca.Start(ctx); err != nil {
		t.Fatalf("client A Start: %v", err)
	}
	cb, err := NewClient(k, addr, 7, "B")
	if err != nil {
		t.Fatalf("NewClient B: %v", err)
	}
	defer cb.Close()
	if err := cb.Start(ctx); err != nil {
		t.Fatalf("client B Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(srv.ActiveClients()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(srv.ActiveClients()); got != 2 {
		t.Fatalf("expected 2 active clients (same Num, diff session), got %d", got)
	}

	// セッション A の Num=7 だけに送る → ca のみ受信、cb には来ないこと。
	if err := srv.SendOutput(1, []byte("for-A"), []transport.ClientID{{Session: "A", Num: 7}}); err != nil {
		t.Fatalf("SendOutput A: %v", err)
	}
	select {
	case chunk := <-ca.Output():
		out := chunk.Data
		if string(out) != "for-A" {
			t.Fatalf("A got %q want for-A", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client A did not receive its output")
	}
	select {
	case chunk := <-cb.Output():
		out := chunk.Data
		t.Fatalf("client B received cross-session output: %q", out)
	case <-time.After(500 * time.Millisecond):
		// 期待どおり B には来ない
	}
}
