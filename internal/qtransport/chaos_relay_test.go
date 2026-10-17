//go:build chaos

package qtransport

// chaosRelay は実 UDP ソケット 2 本の間に挟む簡易カオスプロキシ（1 クライアント・1 サーバー
// 専用の学習型 NAT）。client 側は relay の Addr() へ接続し、relay がそれを実サーバーへ
// 転送する。転送のたびに方向別ポリシーに従いランダムに loss/duplicate/delay を注入する
// （delay をランダム化するだけで、遅延ばらつきが並べ替えとして自然に出る）。
//
// quic.Transport は任意の net.PacketConn を受け取れるが、client.go/server.go は
// net.ListenUDP を直接呼んでおりテスト用の差し込み口がない。本体コードを一切変えず、
// 実ソケット同士の間に本物の UDP プロキシを挟む方式にすることでそれを回避している。

import (
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chaosPolicy は片方向の障害注入率。
type chaosPolicy struct {
	LossRate float64       // 0..1、この確率でパケットを破棄する
	DupRate  float64       // 0..1、この確率で複製をもう1通、別遅延で追加送信する
	MaxDelay time.Duration // 0..MaxDelay のランダム遅延（0 なら遅延なし）
}

type chaosRelay struct {
	conn       *net.UDPConn
	serverAddr *net.UDPAddr
	toServer   chaosPolicy
	toClient   chaosPolicy
	rng        *rand.Rand // 読み取りループ（単一 goroutine）専用なので無ロックでよい

	clientAddr atomic.Pointer[net.UDPAddr]

	closed chan struct{}
	wg     sync.WaitGroup // 遅延送信の goroutine（Close で回収）
}

// newChaosRelay は 127.0.0.1 に UDP ソケットを立て、serverAddr との間で中継を開始する。
// t にクリーンアップは登録しない（呼び出し側が defer relay.Close() する）。
func newChaosRelay(t *testing.T, serverAddr string, seed uint64, toServer, toClient chaosPolicy) *chaosRelay {
	t.Helper()
	sAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		t.Fatalf("chaosRelay: resolve server addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("chaosRelay: listen: %v", err)
	}
	r := &chaosRelay{
		conn:       conn,
		serverAddr: sAddr,
		toServer:   toServer,
		toClient:   toClient,
		rng:        rand.New(rand.NewPCG(seed, seed^0xdeadbeefcafebabe)),
		closed:     make(chan struct{}),
	}
	go r.loop()
	return r
}

func (r *chaosRelay) Addr() string { return r.conn.LocalAddr().String() }

func (r *chaosRelay) Close() error {
	close(r.closed)
	err := r.conn.Close()
	r.wg.Wait()
	return err
}

func (r *chaosRelay) loop() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-r.closed:
				return
			default:
				continue
			}
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		if addr.IP.Equal(r.serverAddr.IP) && addr.Port == r.serverAddr.Port {
			// server -> client
			r.forward(pkt, r.toClient, r.clientAddr.Load)
		} else {
			// client -> server（送信元アドレスを学習する。migration/reconnect で
			// 新しいローカルソケットに変わっても、直近の送信元を追従するだけでよい）
			r.clientAddr.Store(addr)
			r.forward(pkt, r.toServer, func() *net.UDPAddr { return r.serverAddr })
		}
	}
}

func (r *chaosRelay) forward(pkt []byte, policy chaosPolicy, dstFn func() *net.UDPAddr) {
	if r.rng.Float64() < policy.LossRate {
		return
	}
	send := func(delay time.Duration) {
		r.wg.Add(1)
		time.AfterFunc(delay, func() {
			defer r.wg.Done()
			if dst := dstFn(); dst != nil {
				_, _ = r.conn.WriteToUDP(pkt, dst)
			}
		})
	}
	delay := func() time.Duration {
		if policy.MaxDelay <= 0 {
			return 0
		}
		return time.Duration(r.rng.Int64N(int64(policy.MaxDelay) + 1))
	}
	send(delay())
	if r.rng.Float64() < policy.DupRate {
		send(delay())
	}
}
