package session

// 共有モード（--udp-port）で、あるセッションの PTY が入力を読まずに詰まっても、他のセッションの
// 入力は流れ続けること。以前は全セッションの入力を 1 goroutine が直列に PTY へ書いていたため、
// PTY の入力バッファ（約 18KB）を超える先行入力で全セッションの入力が止まっていた。

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/qtransport"
)

func TestSharedTransport_StuckSessionDoesNotBlockOthers(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	mgr := NewManager()
	defer mgr.Close()
	if err := mgr.InitSharedTransport(port); err != nil {
		t.Fatalf("InitSharedTransport: %v", err)
	}
	stuck, err := mgr.CreateSession("stuck", "/bin/sleep", []string{"30"}, nil, "", 24, 80, false)
	if err != nil {
		t.Fatalf("CreateSession(stuck): %v", err)
	}
	live, err := mgr.CreateSession("live", "/bin/cat", nil, nil, "", 24, 80, false)
	if err != nil {
		t.Fatalf("CreateSession(live): %v", err)
	}
	if err := stuck.StartProcess(); err != nil {
		t.Fatalf("StartProcess(stuck): %v", err)
	}
	if err := live.StartProcess(); err != nil {
		t.Fatalf("StartProcess(live): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := func(sess *Session, num uint16) *qtransport.Client {
		t.Helper()
		if err := sess.RegisterQUICClient(num); err != nil {
			t.Fatalf("RegisterQUICClient: %v", err)
		}
		cli := qtransport.NewClient(qtransport.ClientConfig{
			Key:        mgr.GetSharedKey(),
			Candidates: []string{"127.0.0.1:" + strconv.Itoa(port)},
			ClientID:   num,
			SessionID:  sess.ID,
		})
		if err := cli.Start(ctx); err != nil {
			t.Fatalf("client Start: %v", err)
		}
		t.Cleanup(func() { cli.Close() })
		return cli
	}
	stuckCli := dial(stuck, 1)
	liveCli := dial(live, 2)

	// sleep は stdin を読まないので、PTY の入力バッファを超えた分で書き込みが詰まる
	if err := stuckCli.SendInput(bytes.Repeat([]byte(strings.Repeat("x", 99)+"\n"), 640)); err != nil {
		t.Fatalf("SendInput(stuck): %v", err)
	}
	time.Sleep(300 * time.Millisecond) // サーバが stuck への書き込みで詰まるまで待つ

	if err := liveCli.SendInput([]byte("still-alive\n")); err != nil {
		t.Fatalf("SendInput(live): %v", err)
	}
	var got []byte
	deadline := time.After(3 * time.Second)
	for !bytes.Contains(got, []byte("still-alive")) {
		select {
		case ch := <-liveCli.Output():
			got = append(got, ch.Data...)
		case <-deadline:
			t.Fatalf("input to another session was blocked by the stuck session (live output: %q)", got)
		}
	}
}
