package qtransport

// 長スリープ（idle timeout 超で接続死）→ full reconnect の検証。
// reconnect 後も lastOffset が保持され、Hello 経由で OnResyncNeeded が
// 正しい fromOffset で呼ばれ、断中に積まれた出力（backlog）が届くこと。

import (
	"context"
	"crypto/rand"
	"net"
	"sync"
	"sync/atomic"
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

	var mu sync.Mutex
	backlog := []transport.OutputChunk{{Offset: 1, Data: []byte("aaa")}}
	var resyncFrom atomic.Uint64
	srv.OnResyncNeeded(func(_ transport.ClientID, fromOffset uint64) ([]transport.OutputChunk, error) {
		resyncFrom.Store(fromOffset)
		mu.Lock()
		defer mu.Unlock()
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

	// 断中に出力が積まれた状況を模擬（backlog に offset=2 追加）。
	mu.Lock()
	backlog = append(backlog, transport.OutputChunk{Offset: 2, Data: []byte("bbb")})
	mu.Unlock()

	// 接続死を模擬して full reconnect（lastOffset=1 を保持して Hello）。
	if err := qc.reconnect("test"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}

	// reconnect の resync は fromOffset = lastOffset(1)+1 = 2 で呼ばれ "bbb" が届く。
	recv("bbb")
	if got := resyncFrom.Load(); got != 2 {
		t.Fatalf("reconnect OnResyncNeeded fromOffset=%d want 2", got)
	}
}
