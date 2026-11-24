package qtransport

// 経路が黙って切れている間（パケットが消えるだけで接続断は検知されない）でも、SendInput が
// 期限なしにブロックせず、詰まった後は素早く失敗すること（stdin 処理と Ctrl-^ を止めない）。
// 中継は liveness_test.go の blackholeRelay を使う。

import (
	"context"
	"crypto/rand"
	"testing"
	"time"
)

func TestSendInput_DoesNotBlockOnDeadPath(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	inputs := inputChan(srv)
	go func() {
		for range inputs {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	relay := newBlackholeRelay(t, srv.Addr().String())
	cli := NewClient(ClientConfig{Key: k, Candidates: []string{relay.Addr()}, ClientID: 7})
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	if err := cli.SendInput([]byte("hello")); err != nil {
		t.Fatalf("SendInput before blackhole: %v", err)
	}

	relay.drop.Store(true)
	chunk := make([]byte, 4096)
	start := time.Now()
	done := make(chan struct{})
	var failed int
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			if cli.SendInput(chunk) != nil {
				failed++
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(inputWriteTimeout + 5*time.Second):
		t.Fatalf("SendInput blocked on a dead path (20 writes not done after %v)", time.Since(start))
	}
	if failed == 0 {
		t.Error("no SendInput failed on a dead path")
	}
	t.Logf("20 writes on a dead path took %v (%d failed)", time.Since(start).Truncate(time.Millisecond), failed)
}
