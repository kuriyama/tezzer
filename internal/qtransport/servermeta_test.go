package qtransport

// server→client 制御チャネル（control bidi）の検証: 接続時に ServerMeta が届き、
// SendSessionGone が OnSessionNotFound へ配送されること（UDS 非依存の QUIC 経路）。

import (
	"context"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

func TestQUICTransport_ServerMetaAndSessionGone(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	srv.SetServerMeta("build-xyz", "2026-06-27", []byte{1, 2, 3, 4})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	cli, err := NewClient(k, addr, 7, "s1")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()

	metaCh := make(chan [3]string, 1)
	cli.OnServerMeta(func(buildID, buildTime string, instanceID []byte) {
		metaCh <- [3]string{buildID, buildTime, string(instanceID)}
	})
	type goneEvent struct {
		reason   string
		exitCode int
	}
	goneCh := make(chan goneEvent, 2)
	cli.OnSessionNotFound(func(reason string, exitCode int) { goneCh <- goneEvent{reason, exitCode} })
	statusCh := make(chan string, 1)
	cli.OnStatusMessage(func(msg string) { statusCh <- msg })

	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	// 接続時に ServerMeta が届く。
	select {
	case m := <-metaCh:
		if m[0] != "build-xyz" || m[1] != "2026-06-27" || m[2] != string([]byte{1, 2, 3, 4}) {
			t.Fatalf("ServerMeta got %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive ServerMeta on connect")
	}

	// サーバが接続を把握するまで待つ。
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.ActiveClients()) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// SendStatus が OnStatusMessage へ届く（再同期欠損の再描画通知などに使う）。
	if err := srv.SendStatus(transport.ClientID{Session: "s1", Num: 7}, "Output was dropped"); err != nil {
		t.Fatalf("SendStatus: %v", err)
	}
	select {
	case msg := <-statusCh:
		if msg != "Output was dropped" {
			t.Fatalf("OnStatusMessage msg=%q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive status message")
	}

	// SessionGone が OnSessionNotFound へ届く（exit code なし = -1）。
	if err := srv.SendSessionGone(transport.ClientID{Session: "s1", Num: 7}, "killed", -1); err != nil {
		t.Fatalf("SendSessionGone: %v", err)
	}
	select {
	case ev := <-goneCh:
		if ev.reason != "killed" || ev.exitCode != -1 {
			t.Fatalf("OnSessionNotFound got (%q, %d) want (killed, -1)", ev.reason, ev.exitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive session-gone notification")
	}

	// exit code 付きの SessionGone が届く（0 も「不明(-1)」と区別されて伝搬される）。
	if err := srv.SendSessionGone(transport.ClientID{Session: "s1", Num: 7}, "SESSION_CLOSED: PTY session has ended", 0); err != nil {
		t.Fatalf("SendSessionGone with exit code: %v", err)
	}
	select {
	case ev := <-goneCh:
		if ev.exitCode != 0 {
			t.Fatalf("OnSessionNotFound exitCode=%d want 0", ev.exitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive session-gone notification with exit code")
	}
}
