package qtransport

// rforward_test.go: リモートフォワード（-R）の QUIC 側テスト。
// Hello での登録通知・サーバ → クライアントの中継・クライアント側 dial 失敗の拒否・
// 明示的終了と接続喪失の区別・無効化時の非広告を検証する。listener と登録表は
// session.Manager 側（internal/session/rforward_test.go）で検証する。

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

type rfwdHelloEvent struct {
	id     transport.ClientID
	device string
	ports  []uint16
}

type rfwdGoneEvent struct {
	id       transport.ClientID
	explicit bool
}

type rfwdPair struct {
	srv    *quicServer
	cli    *quicClient
	hellos chan rfwdHelloEvent
	gones  chan rfwdGoneEvent
}

// setupRFwdPair はサーバ・クライアントを接続する。targets はクライアントの -R 定義。
func setupRFwdPair(t *testing.T, forwardingEnabled bool, targets map[uint16]string) *rfwdPair {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)
	st, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := st.(*quicServer)
	srv.SetTCPForwarding(forwardingEnabled)
	p := &rfwdPair{srv: srv, hellos: make(chan rfwdHelloEvent, 8), gones: make(chan rfwdGoneEvent, 8)}
	srv.OnRemoteForward(
		func(id transport.ClientID, device string, ports []uint16) {
			p.hellos <- rfwdHelloEvent{id, device, ports}
		},
		func(id transport.ClientID, explicit bool) { p.gones <- rfwdGoneEvent{id, explicit} },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	ct, err := NewClient(k, srv.Addr().String(), 1, "sess")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	p.cli = ct.(*quicClient)
	p.cli.SetRemoteForwards("0123456789abcdef0123456789abcdef", targets)
	t.Cleanup(func() { p.cli.Close() })
	if err := p.cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	return p
}

func waitHello(t *testing.T, p *rfwdPair) rfwdHelloEvent {
	t.Helper()
	select {
	case ev := <-p.hellos:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no -R hello")
	}
	return rfwdHelloEvent{}
}

func TestRemoteForward_RelayToClientTarget(t *testing.T) {
	echo := startTCPEchoServer(t)
	p := setupRFwdPair(t, true, map[uint16]string{9222: echo.Addr().String(), 80: "127.0.0.1:1"})

	ev := waitHello(t, p)
	if ev.device != "0123456789abcdef0123456789abcdef" || len(ev.ports) != 2 || ev.ports[0] != 80 || ev.ports[1] != 9222 {
		t.Fatalf("hello = %+v, want device and sorted ports [80 9222]", ev)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !p.cli.RemoteForwardingSupported() {
		if time.Now().After(deadline) {
			t.Fatal("client did not learn -R feature")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fc, err := p.srv.OpenRemoteForward(ctx, ev.id, 9222)
	if err != nil {
		t.Fatalf("OpenRemoteForward: %v", err)
	}
	defer fc.Close()
	if _, err := fc.Write([]byte("hello cdp")); err != nil {
		t.Fatal(err)
	}
	_ = fc.CloseWrite()
	got, err := io.ReadAll(fc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello cdp" {
		t.Fatalf("echo = %q", got)
	}
}

func TestRemoteForward_ClientRejects(t *testing.T) {
	// 1 番ポートは listen していない想定（dial 失敗）。
	p := setupRFwdPair(t, true, map[uint16]string{9222: "127.0.0.1:1"})
	ev := waitHello(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := p.srv.OpenRemoteForward(ctx, ev.id, 9222); !errors.Is(err, transport.ErrForwardRejected) {
		t.Fatalf("dial failure: err = %v, want ErrForwardRejected", err)
	}
	// 定義のないポートも拒否される（サーバがクライアントに任意の dial 先を指示できない）。
	if _, err := p.srv.OpenRemoteForward(ctx, ev.id, 9999); !errors.Is(err, transport.ErrForwardRejected) {
		t.Fatalf("unknown port: err = %v, want ErrForwardRejected", err)
	}
}

func TestRemoteForward_GoneExplicitOnClose(t *testing.T) {
	p := setupRFwdPair(t, true, map[uint16]string{9222: "127.0.0.1:1"})
	ev := waitHello(t, p)
	p.cli.Close()
	select {
	case g := <-p.gones:
		if g.id != ev.id || !g.explicit {
			t.Fatalf("gone = %+v, want explicit for %v", g, ev.id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no gone after client Close")
	}
}

func TestRemoteForward_GoneNotExplicitOnLoss(t *testing.T) {
	p := setupRFwdPair(t, true, map[uint16]string{9222: "127.0.0.1:1"})
	ev := waitHello(t, p)
	// reconnect と同じコード（0）で閉じる = 意図した終了ではない。
	_ = p.cli.conn.CloseWithError(0, "lost")
	select {
	case g := <-p.gones:
		if g.id != ev.id || g.explicit {
			t.Fatalf("gone = %+v, want non-explicit", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no gone after connection loss")
	}
}

func TestRemoteForward_DisabledNotAdvertised(t *testing.T) {
	p := setupRFwdPair(t, false, map[uint16]string{9222: "127.0.0.1:1"})
	select {
	case ev := <-p.hellos:
		t.Fatalf("hello delivered while forwarding disabled: %+v", ev)
	case <-time.After(500 * time.Millisecond):
	}
	if p.cli.RemoteForwardingSupported() {
		t.Fatal("-R advertised while forwarding disabled")
	}
	if _, err := p.srv.OpenRemoteForward(context.Background(), transport.ClientID{Session: "sess", Num: 1}, 9222); err == nil {
		t.Fatal("OpenRemoteForward succeeded while forwarding disabled")
	}
}

// -R を指定しないクライアントの Hello では登録通知が来ない。
func TestRemoteForward_NoHelloWithoutSpecs(t *testing.T) {
	p := setupRFwdPair(t, true, nil)
	select {
	case ev := <-p.hellos:
		t.Fatalf("unexpected hello: %+v", ev)
	case <-time.After(500 * time.Millisecond):
	}
}

// 長すぎる DeviceID の Hello は -R として扱わない（登録通知も来ない）。
func TestRemoteForward_DeviceIDTooLong(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	st, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := st.(*quicServer)
	srv.OnRemoteForward(func(transport.ClientID, string, []uint16) {}, func(transport.ClientID, bool) {})
	if h := srv.rfwdHelloFor(&ctrlMsg{RemoteForwards: []uint16{9222}, DeviceID: strings.Repeat("a", maxDeviceIDLen+1)}); h != nil {
		t.Fatal("over-long device ID accepted")
	}
	if h := srv.rfwdHelloFor(&ctrlMsg{RemoteForwards: []uint16{9222}, DeviceID: strings.Repeat("a", 32)}); h == nil {
		t.Fatal("normal device ID rejected")
	}
}
