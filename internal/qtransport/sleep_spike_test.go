package qtransport

// Spike C: 長スリープ（device sleep）からの復帰。
//
// 実時間で数時間待てないので、スリープ＝「一定時間パケットを一切流さない」を
// ゲート付き PacketConn で模擬する（radio off 相当）。idle timeout を短く設定し、
// スリープ長との大小で挙動を確認する:
//   - C1: sleep < MaxIdleTimeout → 接続生存・同一ストリームで再開
//   - C2: sleep > MaxIdleTimeout → 接続死（(b) モード: 0-RTT 再接続＋アプリ層再同期が要る）
//   - C3: sleep（< idle）後に新ネットワークへ migration（シナリオ2: 閉じて移動して開く）
//
// 実時間スリープを含むため -short ではスキップする（make ci を遅くしない）。

import (
	"bufio"
	"context"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// gatedConn は net.PacketConn を「スリープ中はパケットを一切通さない」ようゲートする。
// *net.UDPConn を named field で保持し、OOB メソッド（ReadMsgUDP 等）を昇格させない
// （昇格すると quic-go が OOB 経路を使い ReadFrom ゲートを迂回するため）。
type gatedConn struct {
	inner   *net.UDPConn
	blocked atomic.Bool
}

func newGatedConn(t *testing.T) *gatedConn {
	t.Helper()
	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	return &gatedConn{inner: uc}
}

func (g *gatedConn) setBlocked(b bool) { g.blocked.Store(b) }

func (g *gatedConn) ReadFrom(p []byte) (int, net.Addr, error) {
	// 注意: deadline は一切いじらない（quic-go は Transport.Close 時に
	// SetReadDeadline(now) で read ループを抜けさせるため、こちらで打ち消すと
	// シャットダウンがハングする）。スリープ中は届いたパケットを捨ててループし、
	// パケットが来なければ inner.ReadFrom が自然にブロックする（＝無通信）。
	for {
		n, addr, err := g.inner.ReadFrom(p)
		if err != nil {
			return n, addr, err // timeout(シャットダウン) や実エラーは伝播
		}
		if g.blocked.Load() {
			continue // スリープ中: 受信パケットを破棄
		}
		return n, addr, nil
	}
}

func (g *gatedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if g.blocked.Load() {
		return len(p), nil // スリープ中: 送信は黙って捨てる（送れたふり）
	}
	return g.inner.WriteTo(p, addr)
}

func (g *gatedConn) Close() error                       { return g.inner.Close() }
func (g *gatedConn) LocalAddr() net.Addr                { return g.inner.LocalAddr() }
func (g *gatedConn) SetDeadline(t time.Time) error      { return g.inner.SetDeadline(t) }
func (g *gatedConn) SetReadDeadline(t time.Time) error  { return g.inner.SetReadDeadline(t) }
func (g *gatedConn) SetWriteDeadline(t time.Time) error { return g.inner.SetWriteDeadline(t) }

// startEchoServer は接続を受けてストリームをバイトエコーするサーバを起動する。
func startEchoServer(t *testing.T, k []byte, conf *quic.Config) *quic.Listener {
	t.Helper()
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLSForKey(t, k, k), conf)
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func(conn *quic.Conn) {
				str, err := conn.AcceptStream(context.Background())
				if err != nil {
					return
				}
				_, _ = io.Copy(str, str) // バイトエコー
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func mustRoundTrip(t *testing.T, str *quic.Stream, br *bufio.Reader, msg string) {
	t.Helper()
	_ = str.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := str.Write([]byte(msg + "\n")); err != nil {
		t.Fatalf("write %q: %v", msg, err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo of %q: %v", msg, err)
	}
	if got := strings.TrimRight(line, "\n"); got != msg {
		t.Fatalf("echo mismatch: got %q want %q", got, msg)
	}
}

// C1: sleep < idle → 接続生存・同一ストリームで再開。
func TestSpikeC_SleepWithinIdle_Survives(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time sleep")
	}
	k := make([]byte, 32)
	rand.Read(k)
	conf := &quic.Config{MaxIdleTimeout: 4 * time.Second, KeepAlivePeriod: 1 * time.Second}
	ln := startEchoServer(t, k, conf)

	gc := newGatedConn(t)
	tr := &quic.Transport{Conn: gc}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := tr.Dial(ctx, ln.Addr(), clientTLSForKey(t, k, k), conf)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseWithError(0, "done")
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	br := bufio.NewReader(str)

	mustRoundTrip(t, str, br, "before-sleep")

	// スリープ 1.5s（< idle 4s）
	gc.setBlocked(true)
	time.Sleep(1500 * time.Millisecond)
	gc.setBlocked(false)

	mustRoundTrip(t, str, br, "after-sleep")
	t.Log("C1 OK: connection survived sleep within idle timeout, same stream resumed")
}

// C2: sleep > idle → 接続死（resumption が必要なモード(b) の実証）。
func TestSpikeC_SleepBeyondIdle_Dies(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time sleep")
	}
	k := make([]byte, 32)
	rand.Read(k)
	conf := &quic.Config{MaxIdleTimeout: 2 * time.Second, KeepAlivePeriod: 1 * time.Second}
	ln := startEchoServer(t, k, conf)

	gc := newGatedConn(t)
	tr := &quic.Transport{Conn: gc}
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := tr.Dial(ctx, ln.Addr(), clientTLSForKey(t, k, k), conf)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseWithError(0, "done")
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	mustRoundTrip(t, str, bufio.NewReader(str), "before-sleep")

	// スリープ 3.5s（> idle 2s）→ idle timeout で接続が死ぬはず
	gc.setBlocked(true)
	time.Sleep(3500 * time.Millisecond)
	gc.setBlocked(false)

	select {
	case <-conn.Context().Done():
		t.Logf("C2 OK: connection correctly died after sleep beyond idle: %v", context.Cause(conn.Context()))
	case <-time.After(2 * time.Second):
		t.Fatal("connection unexpectedly still alive after sleep beyond idle timeout")
	}
}

// C3: sleep（< idle）後に新ネットワークへ migration（シナリオ2: 閉じて移動して開く）。
func TestSpikeC_SleepThenMigrateNewNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time sleep")
	}
	k := make([]byte, 32)
	rand.Read(k)
	conf := &quic.Config{MaxIdleTimeout: 6 * time.Second, KeepAlivePeriod: 1 * time.Second}
	ln := startEchoServer(t, k, conf)

	gc1 := newGatedConn(t)
	tr1 := &quic.Transport{Conn: gc1}
	defer tr1.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := tr1.Dial(ctx, ln.Addr(), clientTLSForKey(t, k, k), conf)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseWithError(0, "done")
	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	br := bufio.NewReader(str)
	mustRoundTrip(t, str, br, "before-sleep")
	localBefore := conn.LocalAddr().String()

	// スリープ（旧ネットワークで無通信）1.5s < idle 6s
	gc1.setBlocked(true)
	time.Sleep(1500 * time.Millisecond)
	// 旧ネットワークは戻らない（gc1 は blocked のまま）。新ネットワークで起床 → migrate。
	uconn2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP path2: %v", err)
	}
	tr2 := &quic.Transport{Conn: uconn2}
	defer tr2.Close()

	path, err := conn.AddPath(tr2)
	if err != nil {
		t.Fatalf("AddPath: %v", err)
	}
	if err := path.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if err := path.Switch(); err != nil {
		t.Fatalf("Switch: %v", err)
	}

	mustRoundTrip(t, str, br, "after-wake-on-new-network")
	localAfter := conn.LocalAddr().String()
	if localAfter == localBefore || localAfter != uconn2.LocalAddr().String() {
		t.Fatalf("did not migrate to new network socket: before=%s after=%s want=%s",
			localBefore, localAfter, uconn2.LocalAddr().String())
	}
	t.Logf("C3 OK: sleep then wake on new network %s -> %s, same stream resumed", localBefore, localAfter)
}
