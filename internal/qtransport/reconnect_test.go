package qtransport

// 長スリープ（idle timeout 超で接続死）→ full reconnect の検証。
// reconnect 後も lastOffset が保持され、Hello 経由で OnResyncNeeded が
// 正しい fromOffset で呼ばれ、断中に積まれた出力（backlog）が届くこと。

import (
	"context"
	"crypto/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

func TestQUICTransport_ReconnectResync(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	// バッチ契約（transport は空が返るまで offset+1 で繰り返し呼ぶ）に合わせ、
	// 呼び出しごとの fromOffset を列で記録する。
	var mu sync.Mutex
	backlog := []transport.OutputChunk{{Offset: 1, Data: []byte("aaa")}}
	var resyncCalls []uint64
	srv.OnResyncNeeded(func(_ transport.ClientID, fromOffset uint64) ([]transport.OutputChunk, error) {
		mu.Lock()
		defer mu.Unlock()
		resyncCalls = append(resyncCalls, fromOffset)
		var out []transport.OutputChunk
		for _, ch := range backlog {
			if ch.Offset >= fromOffset {
				out = append(out, ch)
			}
		}
		return out, nil
	})

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

	recv := func(want string) {
		t.Helper()
		select {
		case chunk := <-cli.Output():
			out := chunk.Data
			if string(out) != want {
				t.Fatalf("got %q want %q", out, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for %q", want)
		}
	}

	// fresh connect: resync from 1 → "aaa"（lastOffset=1 へ）。
	recv("aaa")

	// 初回 resync の呼び出し列（1 → 空応答の 2 で終了）が落ち着くのを待つ。
	waitCalls := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			l := len(resyncCalls)
			mu.Unlock()
			if l >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout waiting for %d resync calls (got %d)", n, l)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitCalls(2)

	// 断中に出力が積まれた状況を模擬（backlog に offset=2 追加）。
	mu.Lock()
	backlog = append(backlog, transport.OutputChunk{Offset: 2, Data: []byte("bbb")})
	mu.Unlock()

	// 接続死を模擬して full reconnect（lastOffset=1 を保持して Hello）。
	if err := qc.reconnect("test"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	// reconnect の resync は fromOffset = lastOffset(1)+1 = 2 で始まり "bbb" が届く。
	recv("bbb")
	waitCalls(3)
	mu.Lock()
	reconnectFrom := resyncCalls[2]
	mu.Unlock()
	if reconnectFrom != 2 {
		t.Fatalf("reconnect OnResyncNeeded first fromOffset=%d want 2", reconnectFrom)
	}
}

// reconnect 連続失敗時の指数バックオフ（bump で倍々・上限で頭打ち・reset で即時復帰可）。
func TestReconnectBackoff(t *testing.T) {
	c := &quicClient{}
	if c.inBackoff() {
		t.Fatal("fresh client should not be in backoff")
	}
	if d := c.bumpBackoff(); d != reconnectBackoffBase {
		t.Fatalf("first backoff = %v, want %v", d, reconnectBackoffBase)
	}
	if !c.inBackoff() {
		t.Fatal("should be in backoff after failure")
	}
	if d := c.bumpBackoff(); d != 2*reconnectBackoffBase {
		t.Fatalf("second backoff = %v, want %v", d, 2*reconnectBackoffBase)
	}
	var last time.Duration
	for i := 0; i < 40; i++ { // 上限到達後も頭打ちのまま増えない・オーバーフローしない
		last = c.bumpBackoff()
	}
	if last != reconnectBackoffMax {
		t.Fatalf("capped backoff = %v, want %v", last, reconnectBackoffMax)
	}
	c.resetBackoff()
	if c.inBackoff() {
		t.Fatal("reset should clear backoff")
	}
	if d := c.bumpBackoff(); d != reconnectBackoffBase {
		t.Fatalf("backoff after reset = %v, want %v", d, reconnectBackoffBase)
	}
}
