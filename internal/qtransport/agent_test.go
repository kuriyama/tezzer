package qtransport

// agent_test.go: SSH agent forwarding（-A）の end-to-end テスト。
// forward_test.go と対称（サーバ → クライアント方向の中継）だが、
// provider（agent forwarding を要求したクライアント）が登録されるまでの
// 非同期タイミングがあるため OpenAgentStream はリトライしながら呼ぶ。

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startUnixEchoServer は受けたバイトをそのまま返す Unix ドメインソケットサーバを起動する。
func startUnixEchoServer(t *testing.T) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("unix listen: %v", err)
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
	return sockPath
}

// setupAgentPair はサーバ・クライアントを接続し、クライアントを agent provider として
// 登録した quicServer を返す（sessionID は固定 "sess1"）。
func setupAgentPair(t *testing.T, agentForwardingEnabled bool, clientSockPath string) *quicServer {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)

	srvT, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srvT.Close() })
	srv := srvT.(*quicServer)
	srv.SetAgentForwarding(agentForwardingEnabled)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.Addr().String()

	cliT, err := NewClient(k, addr, 1, "sess1")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { cliT.Close() })
	cli := cliT.(*quicClient)
	if clientSockPath != "" {
		cli.SetAgentSockPath(clientSockPath)
	}
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	return srv
}

// openAgentStreamRetry は provider 登録（Hello の非同期処理）を待ちながら
// OpenAgentStream をリトライする。
func openAgentStreamRetry(t *testing.T, srv *quicServer, sessionID string, timeout time.Duration) (io.ReadWriteCloser, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		fc, err := srv.OpenAgentStream(ctx, sessionID)
		cancel()
		if err == nil {
			return fc, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "no agent forwarding provider") {
			return nil, err // provider 未登録以外のエラーは即座に返す
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

func TestAgent_EchoAndHalfClose(t *testing.T) {
	sockPath := startUnixEchoServer(t)
	srv := setupAgentPair(t, true, sockPath)

	fc, err := openAgentStreamRetry(t, srv, "sess1", 5*time.Second)
	if err != nil {
		t.Fatalf("OpenAgentStream: %v", err)
	}
	defer fc.Close()

	msg := []byte("agent handshake bytes")
	if _, err := fc.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	cw, ok := fc.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("ForwardConn does not implement CloseWrite")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	got, err := io.ReadAll(fc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo got %q want %q", got, msg)
	}
}

func TestAgent_NoProvider(t *testing.T) {
	srv := setupAgentPair(t, true, "") // クライアントは -A なし（sock path 未設定）

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := srv.OpenAgentStream(ctx, "sess1")
	if err == nil {
		t.Fatal("OpenAgentStream should fail when no provider is attached")
	}
}

func TestAgent_ClientDialFailure(t *testing.T) {
	// 存在しないソケットパスを指定 → クライアント側 dial が失敗し ctrlAgentOpenErr が返る。
	srv := setupAgentPair(t, true, filepath.Join(t.TempDir(), "does-not-exist.sock"))

	_, err := openAgentStreamRetry(t, srv, "sess1", 5*time.Second)
	if err == nil {
		t.Fatal("OpenAgentStream should fail when client cannot dial its local socket")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got: %v", err)
	}
}

func TestAgent_Disabled(t *testing.T) {
	sockPath := startUnixEchoServer(t)
	srv := setupAgentPair(t, false, sockPath)

	time.Sleep(200 * time.Millisecond) // Hello 到着猶予（provider 登録自体は起きる）

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := srv.OpenAgentStream(ctx, "sess1")
	if err == nil {
		t.Fatal("OpenAgentStream should fail when server disables agent forwarding")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected disabled error, got: %v", err)
	}
}

func TestAgent_ProviderHandoffOnDisconnect(t *testing.T) {
	sockPath := startUnixEchoServer(t)
	k := make([]byte, 32)
	rand.Read(k)

	srvT, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srvT.Close() })
	srv := srvT.(*quicServer)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.Addr().String()

	// 1台目: provider として接続。
	cli1T, err := NewClient(k, addr, 1, "sess1")
	if err != nil {
		t.Fatalf("NewClient(1): %v", err)
	}
	cli1 := cli1T.(*quicClient)
	cli1.SetAgentSockPath(sockPath)
	if err := cli1.Start(ctx); err != nil {
		t.Fatalf("client1 Start: %v", err)
	}

	if _, err := openAgentStreamRetry(t, srv, "sess1", 5*time.Second); err != nil {
		t.Fatalf("provider1 not registered: %v", err)
	}

	// 2台目: 同じセッションに別クライアントとして -A 付きで attach。
	cli2T, err := NewClient(k, addr, 2, "sess1")
	if err != nil {
		t.Fatalf("NewClient(2): %v", err)
	}
	t.Cleanup(func() { cli2T.Close() })
	cli2 := cli2T.(*quicClient)
	cli2.SetAgentSockPath(sockPath)
	if err := cli2.Start(ctx); err != nil {
		t.Fatalf("client2 Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // Hello 到着猶予（2台目が provider を上書き）

	// 1台目を切断 → provider は2台目に委譲されるはず（消えない）。
	_ = cli1.Close()
	time.Sleep(300 * time.Millisecond) // 切断検知・委譲猶予

	if _, err := openAgentStreamRetry(t, srv, "sess1", 5*time.Second); err != nil {
		t.Fatalf("provider should have handed off to client2: %v", err)
	}
}
