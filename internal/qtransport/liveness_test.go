package qtransport

// liveness（ping/pong）による別 family への早期切替の検証。
// v4 側の経路はブラックホール化できる UDP 中継（127.0.0.1）で、v6 側は ::1 への
// 直接 dial で表現する（サーバーは dual-stack ワイルドカードで両方を受ける）。

import (
	"context"
	"crypto/rand"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

// blackholeRelay は 127.0.0.1 で受けた UDP を serverAddr へ中継する（1 クライアント専用）。
// drop を立てると両方向とも黙って捨てる（片 family の経路断の模擬）。
type blackholeRelay struct {
	conn       *net.UDPConn
	serverAddr *net.UDPAddr
	clientAddr atomic.Pointer[net.UDPAddr]
	drop       atomic.Bool
}

func newBlackholeRelay(t *testing.T, serverAddr string) *blackholeRelay {
	t.Helper()
	sAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		t.Fatalf("relay: resolve: %v", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("relay: listen: %v", err)
	}
	r := &blackholeRelay{conn: conn, serverAddr: sAddr}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if r.drop.Load() {
				continue
			}
			if from.IP.Equal(sAddr.IP) && from.Port == sAddr.Port {
				if c := r.clientAddr.Load(); c != nil {
					_, _ = conn.WriteToUDP(buf[:n], c)
				}
				continue
			}
			r.clientAddr.Store(from)
			_, _ = conn.WriteToUDP(buf[:n], sAddr)
		}
	}()
	return r
}

func (r *blackholeRelay) Addr() string { return r.conn.LocalAddr().String() }

// setupLivenessPair は dual-stack のエコーサーバーと、中継経由（v4）で接続済みの
// クライアントを返す。::1 が使えない環境では skip する。
func setupLivenessPair(t *testing.T) (qc *quicClient, relay *blackholeRelay, port int) {
	t.Helper()
	if c, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback}); err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	} else {
		_ = c.Close()
	}

	origInterval, origTimeout := livenessPingInterval.Get(), livenessTimeout.Get()
	livenessPingInterval.Set(100 * time.Millisecond)
	livenessTimeout.Set(500 * time.Millisecond)
	t.Cleanup(func() {
		livenessPingInterval.Set(origInterval)
		livenessTimeout.Set(origTimeout)
	})

	k := make([]byte, 32)
	rand.Read(k)
	srv, err := NewServer(k, ":0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	var off uint64
	go func() {
		for in := range srv.Input() {
			off++
			_ = srv.SendOutput(off, in.Data, []transport.ClientID{in.Client})
		}
	}()
	port = srv.(interface{ Addr() net.Addr }).Addr().(*net.UDPAddr).Port

	relay = newBlackholeRelay(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	cli, err := NewClient(k, relay.Addr(), 1, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	qc = cli.(*quicClient)
	waitFor(t, 5*time.Second, "server features", func() bool {
		return qc.serverFeatures.Load()&FeatureLiveness != 0
	})
	return qc, relay, port
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func echo(t *testing.T, qc *quicClient, msg string) {
	t.Helper()
	if err := qc.SendInput([]byte(msg)); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	select {
	case ch := <-qc.Output():
		if string(ch.Data) != msg {
			t.Fatalf("echo got %q want %q", ch.Data, msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for echo %q", msg)
	}
}

func connAddrOf(qc *quicClient) string {
	qc.migrateMu.Lock()
	defer qc.migrateMu.Unlock()
	return qc.connAddr
}

// v4 経路が死んだら pong 途絶を検知して v6 候補へ乗り換え、ストリームが継続すること。
func TestLiveness_FailoverToOtherFamily(t *testing.T) {
	qc, relay, port := setupLivenessPair(t)
	v6 := net.JoinHostPort("::1", strconv.Itoa(port))
	qc.SetRoamingCandidates([]string{relay.Addr(), v6})

	echo(t, qc, "before")
	waitFor(t, 2*time.Second, "first pong", func() bool { return qc.lastPongNano.Load() != 0 })

	relay.drop.Store(true)
	waitFor(t, 5*time.Second, "failover to v6", func() bool { return connAddrOf(qc) == v6 })
	if s := qc.State(); s != transport.StateConnected {
		t.Fatalf("state after failover = %v, want connected", s)
	}
	echo(t, qc, "after")
}

// 別 family の候補も死んでいる（両方断）なら、接続・状態・バックオフに触れず、
// 経路が戻れば同じ接続で継続すること。
func TestLiveness_BothDownKeepsConn(t *testing.T) {
	qc, relay, _ := setupLivenessPair(t)
	// 誰も読まない v6 ソケット = 応答しない v6 候補
	dead, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatalf("listen dead v6: %v", err)
	}
	defer dead.Close()
	qc.SetRoamingCandidates([]string{relay.Addr(), dead.LocalAddr().String()})
	echo(t, qc, "before")

	orig := connAddrOf(qc)
	relay.drop.Store(true)
	time.Sleep(3 * livenessTimeout.Get())
	if s := qc.State(); s != transport.StateConnected {
		t.Fatalf("state during outage = %v, want connected (unchanged)", s)
	}
	qc.backoffMu.Lock()
	fails := qc.backoffFails
	qc.backoffMu.Unlock()
	if fails != 0 {
		t.Fatalf("backoffFails = %d, want 0", fails)
	}

	relay.drop.Store(false)
	echo(t, qc, "after")
	if got := connAddrOf(qc); got != orig {
		t.Fatalf("connAddr = %q, want unchanged %q", got, orig)
	}
}

// 別 family の候補がなければ ping を打たない（single stack）。
func TestLiveness_NoPingWithoutOtherFamily(t *testing.T) {
	qc, _, _ := setupLivenessPair(t)
	echo(t, qc, "x")
	time.Sleep(5 * livenessPingInterval.Get())
	if qc.lastPongNano.Load() != 0 {
		t.Fatal("pong received although no other-family candidate exists")
	}
}

func TestOtherFamilyCandidates(t *testing.T) {
	cands := []string{"192.0.2.1:7022", "[2001:db8::1]:7022", "198.51.100.1:7022", "example.com:7022"}
	tests := []struct {
		current string
		want    []string
	}{
		{"192.0.2.1:7022", []string{"[2001:db8::1]:7022"}},
		{"[2001:db8::1]:7022", []string{"192.0.2.1:7022", "198.51.100.1:7022"}},
		{"example.com:7022", nil},
	}
	for _, tt := range tests {
		got := otherFamilyCandidates(cands, tt.current)
		if len(got) != len(tt.want) {
			t.Fatalf("current=%s: got %v want %v", tt.current, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("current=%s: got %v want %v", tt.current, got, tt.want)
			}
		}
	}
}
