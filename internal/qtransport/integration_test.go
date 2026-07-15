package qtransport

// qtransport の server/client 実装を transport インターフェース経由で end-to-end 検証する
// （単一クライアント・v1 プロトコル: Hello / 出力 / 入力 / Resize）。

import (
	"context"
	"crypto/rand"
	"github.com/kuriyama/tezzer/internal/transport"
	"net"
	"testing"
	"time"
)

func TestQUICTransport_EndToEnd(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	connected := make(chan uint16, 1)
	srv.OnClientConnect(func(id transport.ClientID) { connected <- id.Num })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}

	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	const clientID = uint16(4242)
	cli, err := NewClient(k, addr, clientID, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	// OnClientConnect が clientID 付きで発火する
	select {
	case id := <-connected:
		if id != clientID {
			t.Fatalf("OnClientConnect id=%d want %d", id, clientID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not report client connect")
	}

	// 入力: client → server
	if err := cli.SendInput([]byte("hello-input")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	select {
	case in := <-srv.Input():
		if in.Client.Num != clientID || string(in.Data) != "hello-input" {
			t.Fatalf("server Input got {id=%d data=%q}", in.Client.Num, in.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive input")
	}

	// 出力: server → client（fan-out 1 件）
	if err := srv.SendOutput(1, []byte("hello-output"), []transport.ClientID{{Num: clientID}}); err != nil {
		t.Fatalf("SendOutput: %v", err)
	}
	select {
	case chunk := <-cli.Output():
		out := chunk.Data
		if string(out) != "hello-output" {
			t.Fatalf("client Output got %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not receive output")
	}

	// Resize: client → server（control フレーム）
	if err := cli.SendResize(120, 40); err != nil {
		t.Fatalf("SendResize: %v", err)
	}
	select {
	case rz := <-srv.Resize():
		if rz.Client.Num != clientID || rz.Cols != 120 || rz.Rows != 40 {
			t.Fatalf("server Resize got %+v", rz)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive resize")
	}

	if got := srv.ActiveClients(); len(got) != 1 || got[0].Num != clientID {
		t.Fatalf("ActiveClientIDs=%v want [%d]", got, clientID)
	}
}
