package session

// QUIC で接続中のクライアントには UDS へ出力を送らないこと（SSH 側の帯域を二重にしない）。
// UDS クライアントと QUIC の client ID は UDP_CLIENT_INFO（SetQUICClientID）で結び付く。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/qtransport"
)

func TestHandlePTYOutput_SkipsUDSForActiveQUICClient(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()
	sess, err := mgr.CreateSession("uds-suppress", "/bin/cat", nil, nil, "", 24, 80, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	withQUIC := sess.AttachClient(0, "TEST", "with-quic", 0)
	udsOnly := sess.AttachClient(0, "TEST", "uds-only", 0)
	const num = 4242
	if err := sess.RegisterQUICClient(num); err != nil {
		t.Fatalf("RegisterQUICClient: %v", err)
	}
	withQUIC.SetQUICClientID(num)

	cli := qtransport.NewClient(qtransport.ClientConfig{
		Key:        sess.GetQUICKey(),
		Candidates: []string{"127.0.0.1:" + strconv.Itoa(sess.GetQUICPort())},
		ClientID:   num,
		SessionID:  sess.ID,
	})
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("QUIC client Start: %v", err)
	}
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	// サーバ側で接続が active になるまで待つ
	deadline := time.Now().Add(3 * time.Second)
	for len(sess.st.ActiveClients()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("QUIC client did not become active")
		}
		time.Sleep(10 * time.Millisecond)
	}
	drainOutput(t, withQUIC.OutCh, 100*time.Millisecond)
	drainOutput(t, udsOnly.OutCh, 100*time.Millisecond)

	sess.handlePTYOutput([]byte("marker-output"))

	if got := drainOutput(t, withQUIC.OutCh, 200*time.Millisecond); len(got) != 0 {
		t.Errorf("UDS output sent to a client connected over QUIC: %q", got)
	}
	waitForEcho(t, udsOnly.OutCh, "marker-output", 2*time.Second)
	select {
	case ch := <-cli.Output():
		if string(ch.Data) != "marker-output" {
			t.Errorf("QUIC output = %q, want %q", ch.Data, "marker-output")
		}
	case <-time.After(2 * time.Second):
		t.Error("no output over QUIC")
	}
}
