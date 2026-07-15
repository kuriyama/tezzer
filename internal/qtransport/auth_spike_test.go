// Package qtransport は QUIC トランスポート移行の feasibility 検証用スパイク置き場。
// 本実装ではなく、docs/dev/quic-transport-feasibility.md の spike を実コードで確認する。
package qtransport

// Spike A: 共有鍵 K に紐づく self-signed mTLS で quic-go 接続が成立するか（PKI 不要の認証）。
//
// 検証内容:
//   - K から HKDF で Ed25519 鍵を導出し、それを載せた自己署名証明書を両端が提示（mTLS）
//   - 各端は VerifyPeerCertificate で「相手証明書の公開鍵が K 由来の期待値と一致」を検証
//   - 実 UDP ソケット上で接続確立 → ストリームでバイト送受信できること（正常系）
//   - 異なる K のクライアントはハンドシェイクで弾かれること（異常系）

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// serverTLSForKey / clientTLSForKey は auth.go の本番ヘルパー（tlsConfig）への薄い
// ラッパー。own/expectPeer を分けられるので異常系（鍵不一致）テストにも使える。
func serverTLSForKey(t *testing.T, ownK, expectPeerK []byte) *tls.Config {
	t.Helper()
	cfg, err := tlsConfig(true, ownK, expectPeerK)
	if err != nil {
		t.Fatalf("tlsConfig(server): %v", err)
	}
	return cfg
}

func clientTLSForKey(t *testing.T, ownK, expectPeerK []byte) *tls.Config {
	t.Helper()
	cfg, err := tlsConfig(false, ownK, expectPeerK)
	if err != nil {
		t.Fatalf("tlsConfig(client): %v", err)
	}
	return cfg
}

// TestSpikeA_SharedKeyMTLS_Success は正しい K を共有する両端が接続し、
// ストリームでバイトを送受信できることを確認する。
func TestSpikeA_SharedKeyMTLS_Success(t *testing.T) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}

	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLSForKey(t, k, k), nil)
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	got := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			srvErr <- err
			return
		}
		str, err := conn.AcceptStream(ctx)
		if err != nil {
			srvErr <- err
			return
		}
		buf, err := io.ReadAll(str)
		if err != nil {
			srvErr <- err
			return
		}
		got <- string(buf)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, ln.Addr().String(), clientTLSForKey(t, k, k), nil)
	if err != nil {
		t.Fatalf("DialAddr (should succeed with matching K): %v", err)
	}
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	if _, err := str.Write([]byte("hello over quic")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	str.Close() // 書き込み終了 → server 側 io.ReadAll が EOF で返る

	select {
	case err := <-srvErr:
		t.Fatalf("server side error: %v", err)
	case s := <-got:
		if s != "hello over quic" {
			t.Fatalf("server received %q, want %q", s, "hello over quic")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for server to receive stream data")
	}
	_ = conn.CloseWithError(0, "done")
}

// TestSpikeA_WrongKey_HandshakeFails は異なる K のクライアントが
// ハンドシェイクで弾かれることを確認する（PKI 無しでも認証が効く）。
func TestSpikeA_WrongKey_HandshakeFails(t *testing.T) {
	serverK := make([]byte, 32)
	clientK := make([]byte, 32)
	rand.Read(serverK)
	rand.Read(clientK) // 別の鍵

	// サーバは「peer は serverK 由来の client 鍵を持つはず」と期待するが、
	// クライアントは clientK を使うので不一致になる。
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLSForKey(t, serverK, serverK), nil)
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	defer ln.Close()

	// accept 側も回しておく（サーバのクライアント cert 検証＝拒否を駆動するため）。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			return // 期待どおり: ハンドシェイク拒否で Accept は成功しない
		}
		// 万一 accept できてもストリームは使えないはず
		if str, err := conn.AcceptStream(ctx); err == nil {
			_, _ = io.Copy(str, str)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, ln.Addr().String(), clientTLSForKey(t, clientK, serverK), nil)
	if err != nil {
		t.Logf("correctly rejected at dial: %v", err)
		return
	}
	// TLS 1.3 の client-optimistic completion のため Dial 自体は成功しうる。
	// サーバ側のクライアント cert 拒否は、接続/ストリームを使った時点で表面化する。
	defer conn.CloseWithError(0, "")
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Logf("correctly rejected at OpenStream: %v", err)
		return
	}
	_ = str.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = str.Write([]byte("ping"))
	str.Close()
	if _, err := io.ReadAll(str); err != nil {
		t.Logf("correctly rejected on stream use: %v", err)
		return
	}
	t.Fatal("stream round-trip unexpectedly succeeded with mismatched shared key")
}
