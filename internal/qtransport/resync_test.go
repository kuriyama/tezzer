package qtransport

// offset 再同期の検証: クライアント接続時に OnResyncNeeded が呼ばれ、返した
// OutputChunk が（ライブ出力に先行して）クライアントへ届くこと。

import (
	"context"
	"crypto/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

func TestQUICTransport_ResyncBacklog(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	// バックログ（OutputRingBuffer 相当）。offset 1..3。
	backlog := []transport.OutputChunk{
		{Offset: 1, Data: []byte("aaa")},
		{Offset: 2, Data: []byte("bbb")},
		{Offset: 3, Data: []byte("ccc")},
	}
	// バッチ契約により空が返るまで繰り返し呼ばれるため、初回の fromOffset のみ記録する。
	var gotFrom atomic.Uint64
	srv.OnResyncNeeded(func(_ transport.ClientID, fromOffset uint64) ([]transport.OutputChunk, error) {
		gotFrom.CompareAndSwap(0, fromOffset)
		var out []transport.OutputChunk
		for _, ch := range backlog {
			if ch.Offset >= fromOffset {
				out = append(out, ch)
			}
		}
		return out, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	cli, err := NewClient(k, addr, 55, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	// fresh クライアント（lastOffset=0）→ サーバは fromOffset=1 で全 backlog を返すはず。
	var acc []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(acc) < 9 {
		select {
		case chunk := <-cli.Output():
			out := chunk.Data
			acc = append(acc, out...)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if string(acc) != "aaabbbccc" {
		t.Fatalf("resync backlog mismatch: got %q want %q", string(acc), "aaabbbccc")
	}
	if gotFrom.Load() != 1 {
		t.Fatalf("OnResyncNeeded fromOffset=%d want 1", gotFrom.Load())
	}

	// 再送（offset<=lastOffset）は重複排除される: backlog を再度ライブ送信しても届かない。
	_ = srv.SendOutput(2, []byte("DUP"), []transport.ClientID{{Num: 55}})
	// 新しい offset は届く。
	_ = srv.SendOutput(4, []byte("ddd"), []transport.ClientID{{Num: 55}})
	select {
	case chunk := <-cli.Output():
		out := chunk.Data
		if string(out) != "ddd" {
			t.Fatalf("expected live offset 4 'ddd', got %q (dup not filtered?)", string(out))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive live offset 4 after resync")
	}
}
