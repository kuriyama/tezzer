package qtransport

// forward_test.go: TCP ポートフォワード（-L）の end-to-end テスト。
// echo サーバへの転送・半クローズ・並行多重・dial 失敗・無効化・接続数制限を検証する。

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

// startTCPEchoServer は受けたバイトをそのまま返す TCP サーバを起動する。
// クライアントの CloseWrite（EOF）を受けたら書き戻しを終えて閉じる。
func startTCPEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// setupForwardPair はサーバ・クライアントを接続し、TCPForwarder を返す。
// forwardingEnabled=false ならサーバ側で転送を無効化する。
func setupForwardPair(t *testing.T, forwardingEnabled bool) transport.TCPForwarder {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	srv.(*quicServer).SetTCPForwarding(forwardingEnabled)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	cli, err := NewClient(k, addr, 1, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { cli.Close() })
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	fw := cli.(transport.TCPForwarder)
	// serverMeta（Features）到着を待つ（有効時のみ。無効時は届いても bit が立たない）。
	if forwardingEnabled {
		deadline := time.Now().Add(5 * time.Second)
		for !fw.ForwardingSupported() {
			if time.Now().After(deadline) {
				t.Fatal("client did not learn forwarding feature")
			}
			time.Sleep(10 * time.Millisecond)
		}
	} else {
		time.Sleep(100 * time.Millisecond) // meta 到着猶予
	}
	return fw
}

func TestForward_EchoAndHalfClose(t *testing.T) {
	echo := startTCPEchoServer(t)
	fw := setupForwardPair(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fs, err := fw.OpenForward(ctx, echo.Addr().String())
	if err != nil {
		t.Fatalf("OpenForward: %v", err)
	}
	defer fs.Close()

	msg := []byte("hello through the tunnel")
	if _, err := fs.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 半クローズ: 書き終わりを FIN で伝える → echo サーバは EOF を見て返送を終える
	if err := fs.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(fs)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo got %q want %q", got, msg)
	}
}

func TestForward_LargeTransfer(t *testing.T) {
	echo := startTCPEchoServer(t)
	fw := setupForwardPair(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fs, err := fw.OpenForward(ctx, echo.Addr().String())
	if err != nil {
		t.Fatalf("OpenForward: %v", err)
	}
	defer fs.Close()

	// 数 MB を流して flow control 下でも欠けず順序どおり届くことを確認する。
	payload := make([]byte, 4<<20)
	rand.Read(payload)
	go func() {
		_, _ = fs.Write(payload)
		_ = fs.CloseWrite()
	}()
	got, err := io.ReadAll(fs)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(payload) || string(got) != string(payload) {
		t.Fatalf("large transfer mismatch: got %d bytes want %d", len(got), len(payload))
	}
}

func TestForward_Concurrent(t *testing.T) {
	echo := startTCPEchoServer(t)
	fw := setupForwardPair(t, true)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			fs, err := fw.OpenForward(ctx, echo.Addr().String())
			if err != nil {
				errs <- fmt.Errorf("open %d: %w", i, err)
				return
			}
			defer fs.Close()
			msg := fmt.Sprintf("stream-%d", i)
			if _, err := fs.Write([]byte(msg)); err != nil {
				errs <- fmt.Errorf("write %d: %w", i, err)
				return
			}
			_ = fs.CloseWrite()
			got, err := io.ReadAll(fs)
			if err != nil || string(got) != msg {
				errs <- fmt.Errorf("echo %d: got %q err %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestForward_DialFailure(t *testing.T) {
	fw := setupForwardPair(t, true)

	// 閉じたポートを先に確保してから解放し、確実に dial 失敗させる。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = fw.OpenForward(ctx, deadAddr)
	if err == nil {
		t.Fatal("OpenForward to dead port should fail")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got: %v", err)
	}
}

func TestForward_Disabled(t *testing.T) {
	echo := startTCPEchoServer(t)
	fw := setupForwardPair(t, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if fw.ForwardingSupported() {
		t.Fatal("disabled server should not advertise forwarding")
	}
	_, err := fw.OpenForward(ctx, echo.Addr().String())
	if err == nil {
		t.Fatal("OpenForward should fail when server disables forwarding")
	}
}

func TestForward_PerClientLimit(t *testing.T) {
	echo := startTCPEchoServer(t)
	fw := setupForwardPair(t, true)

	// 上限まで開く（echo サーバに接続を保持させる）。
	open := make([]transport.ForwardConn, 0, maxForwardsPerClient)
	defer func() {
		for _, fs := range open {
			fs.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < maxForwardsPerClient; i++ {
		fs, err := fw.OpenForward(ctx, echo.Addr().String())
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		open = append(open, fs)
	}

	// 上限超過は拒否される
	if _, err := fw.OpenForward(ctx, echo.Addr().String()); err == nil {
		t.Fatal("open beyond limit should fail")
	} else if !strings.Contains(err.Error(), "too many") {
		t.Fatalf("expected limit error, got: %v", err)
	}

	// 1 本閉じれば再び開ける
	_ = open[0].Close()
	open = open[1:]
	deadline := time.Now().Add(5 * time.Second)
	for {
		fs, err := fw.OpenForward(ctx, echo.Addr().String())
		if err == nil {
			open = append(open, fs)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("open after close should succeed, last err: %v", err)
		}
		time.Sleep(50 * time.Millisecond) // サーバ側の後始末（fwdActive デクリメント）待ち
	}
}
