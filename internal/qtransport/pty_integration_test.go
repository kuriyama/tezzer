package qtransport

// PTY-over-QUIC 縦スライス: 実 PTY（cat）を qtransport 越しに駆動できることを検証する。
// session↔transport の中核ループ（PTY 出力 → SendOutput → client、client 入力 → OnInput → PTY）
// が成立することを示す。
//
// 実プロセス起動を伴うため -short ではスキップ。

import (
	"context"
	"crypto/rand"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestPTYoverQUIC(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real PTY process")
	}
	k := make([]byte, 32)
	rand.Read(k)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// --- サーバ + PTY(cat) ---
	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	inputs := inputChan(srv)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.Addr().String()

	cmd := exec.Command("cat") // stdin をそのまま stdout へ（PTY エコーも乗る）
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty.Start: %v", err)
	}
	defer func() { _ = ptmx.Close(); _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	// PTY 出力 → 接続中クライアントへファンアウト（offset = セッション論理オフセット）。
	var offsetMu sync.Mutex
	var offset uint64
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				offsetMu.Lock()
				offset++
				off := offset
				offsetMu.Unlock()
				data := append([]byte(nil), buf[:n]...)
				_ = srv.SendOutput(off, data, srv.ActiveClients())
			}
			if err != nil {
				return
			}
		}
	}()

	// client 入力 → PTY stdin。
	go func() {
		for {
			select {
			case in := <-inputs:
				_, _ = ptmx.Write(in.Data)
			case <-ctx.Done():
				return
			}
		}
	}()

	// --- クライアント ---
	cli := NewClient(ClientConfig{Key: k, Candidates: []string{addr}, ClientID: 7777})
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	// 接続が active になるのを待つ（出力ファンアウト先に入るまで）。
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.ActiveClients()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// 入力を送り、PTY(cat) のエコーが client の Output に返ることを確認。
	const marker = "hello-from-quic-pty"
	if err := cli.SendInput([]byte(marker + "\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}

	var acc strings.Builder
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case chunk := <-cli.Output():
			out := chunk.Data
			acc.Write(out)
			if strings.Contains(acc.String(), marker) {
				t.Logf("PTY-over-QUIC OK: client received echoed %q through real cat PTY", marker)
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("did not receive PTY echo of %q via QUIC (got %q)", marker, acc.String())
}
