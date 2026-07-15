package qtransport

// recover() の多重起動が recovering フラグで弾かれ、回復回数が膨らまないことを検証する
// （実機で観測した Recoveries=92 の自己増殖ループ＝watchdog が recover を同期呼びして
// ブロック→誤ジャンプ→再 recover、の再発防止。watchdog は非同期化したが、その安全装置で
// ある recovering フラグの dedup をここで担保する）。

import (
	"context"
	"crypto/rand"
	"net"
	"sync"
	"testing"
	"time"
)

func TestQUICTransport_ConcurrentRecoverDedup(t *testing.T) {
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

	cli, err := NewClient(k, addr, 7, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	qc := cli.(*quicClient)

	// 接続を殺してから recover を一斉に多数起動する。
	qc.conn.CloseWithError(0, "dead")
	deadline := time.Now().Add(3 * time.Second)
	for !qc.connDead() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); qc.recover("storm") }()
	}
	wg.Wait()

	// 16 本同時に呼んでも recovering フラグで 1 本だけ走る（高々数回に収まる）。
	if got := cli.Stats().RecoveryCount; got > 2 {
		t.Fatalf("RecoveryCount=%d: 多重 recover が dedup されていない（>2）", got)
	}
}
