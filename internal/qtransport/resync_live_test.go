package qtransport

// 再同期（追い付き）の途中にライブ出力が来ても、追い付き分が欠けずに順番どおり届くこと。
// 以前は登録直後からライブ分が同じストリームへ割り込み、クライアントが offset <= lastOffset を
// 捨てるため、割り込まれた後ろの追い付き分が黙って落ちていた。

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestQUICTransport_LiveOutputDuringResync(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	// セッションのリングバッファ相当。PTY reader と同じく「追記してから SendOutput」する。
	var ringMu sync.Mutex
	var ring []OutputChunk
	const backlog = 10
	for i := uint64(1); i <= backlog; i++ {
		ring = append(ring, OutputChunk{Offset: i, Data: []byte(fmt.Sprintf("<%d>", i))})
	}
	cid := ClientID{Num: 55}
	var live sync.Once
	srv.OnResyncNeeded(func(_ ClientID, from uint64) ([]OutputChunk, error) {
		if from == backlog/2 {
			// 追い付きの途中でライブ出力を 1 つ送る
			live.Do(func() {
				ringMu.Lock()
				ch := OutputChunk{Offset: backlog + 1, Data: []byte(fmt.Sprintf("<%d>", backlog+1))}
				ring = append(ring, ch)
				ringMu.Unlock()
				_ = srv.SendOutput(ch.Offset, ch.Data, []ClientID{cid})
			})
		}
		ringMu.Lock()
		defer ringMu.Unlock()
		for _, ch := range ring {
			if ch.Offset >= from {
				return []OutputChunk{ch}, nil // 1 チャンクずつのバッチにして割り込む隙を作る
			}
		}
		return nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	cli := NewClient(ClientConfig{Key: k, Candidates: []string{srv.Addr().String()}, ClientID: cid.Num})
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	var want, got string
	for i := 1; i <= backlog+1; i++ {
		want += fmt.Sprintf("<%d>", i)
	}
	deadline := time.After(5 * time.Second)
	for len(got) < len(want) {
		select {
		case ch := <-cli.Output():
			got += string(ch.Data)
		case <-deadline:
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
