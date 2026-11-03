package qtransport

import (
	"github.com/kuriyama/tezzer/internal/transport"
	"testing"
	"time"
)

// OnClientExit はクライアントの意図した終了（Close = connCodeClientExit）でだけ呼ばれ、
// 回線断（reconnect で戻ってくる）では呼ばれないことを確かめる。
// 切断処理の完了は、同じ handleConn の後始末で先に呼ばれる -R の gone で待つ。

func TestOnClientExit_FiredOnClose(t *testing.T) {
	p := setupRFwdPair(t, true, map[uint16]string{9222: "127.0.0.1:1"})
	exits := make(chan transport.ClientID, 1)
	p.srv.OnClientExit(func(id transport.ClientID) { exits <- id })
	ev := waitHello(t, p)

	p.cli.Close()
	select {
	case id := <-exits:
		if id != ev.id {
			t.Fatalf("exit id = %v, want %v", id, ev.id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnClientExit not called after client Close")
	}
}

func TestOnClientExit_NotFiredOnLoss(t *testing.T) {
	p := setupRFwdPair(t, true, map[uint16]string{9222: "127.0.0.1:1"})
	exits := make(chan transport.ClientID, 1)
	p.srv.OnClientExit(func(id transport.ClientID) { exits <- id })
	waitHello(t, p)

	// reconnect と同じコード（0）で閉じる = 意図した終了ではない。
	_ = p.cli.conn.Load().CloseWithError(0, "lost")
	select {
	case <-p.gones:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not process the disconnect")
	}
	select {
	case id := <-exits:
		t.Fatalf("OnClientExit called on connection loss: %v", id)
	default:
	}
}
