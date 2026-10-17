package qtransport

// liveness.go: アプリ層の軽量 liveness 確認（ping/pong）による別 family への早期切替。
//
// QUIC の経路死は MaxIdleTimeout（60s）経過まで検知できず、dualstack で片方の
// family（例: v6 回線）だけ落ちた場合も 60 秒強止まっていた（docs/dev/testing.md
// 「v4/v6 経路断のフェイルオーバー実測」）。control ストリームに短い間隔で ping を
// 流し、pong が途絶えたら別 family の候補にだけ dial して、つながれば乗り換える。
//
// 方針:
//   - ping を打つのは、サーバーが FeatureLiveness を広告し、かつ候補リストに
//     現在の接続と別 family の候補がある時だけ。single stack では早期検知しても
//     他に試す先がなく、電波を起こすコスト（モバイル回線）だけがかかるため
//   - 早期検知時は別 family への切替だけを試す。失敗しても現在の接続は閉じず、
//     状態（StateRecovering）もバックオフも変えない。障害は v4/v6 両方まとめて
//     落ちるケースの方が多く、その時に reconnect を早めても失敗とバックオフを
//     積むだけで、短い断なら生き残るはずの接続まで捨ててしまうため。
//     両方死んでいる場合は従来どおり idle timeout → watchdog の recover に任せる
//   - フェイルバック（元の family への復帰）はしない

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/quic-go/quic-go"
)

// liveness 確認の間隔としきい値（テストから短縮できるよう差し替え可能）。
// livenessTimeout の間 pong が来なければ切替を試み、失敗したら同じ間隔をおいて再試行する。
var (
	livenessPingInterval = newAtomicDuration(3 * time.Second)
	livenessTimeout      = newAtomicDuration(10 * time.Second)
)

// livenessLoop は ping の送信と pong 途絶の判定を行う（Start で起動）。
func (c *quicClient) livenessLoop() {
	t := time.NewTicker(livenessPingInterval.Get())
	defer t.Stop()
	var watched *quic.Conn // 監視中の接続（差し替わったら監視をやり直す）
	var since time.Time    // 監視開始 or 直近の切替試行の時刻。zero = 監視していない
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		conn, addr, alts := c.livenessTarget()
		if conn == nil {
			watched, since = nil, time.Time{}
			continue
		}
		now := time.Now()
		if conn != watched || since.IsZero() {
			watched, since = conn, now
		}
		c.sendPing()

		silentSince := since
		if p := time.Unix(0, c.lastPongNano.Load()); p.After(silentSince) {
			silentSince = p
		}
		if silent := now.Sub(silentSince); silent >= livenessTimeout.Get() {
			since = now // 失敗時の再試行も livenessTimeout おきに間引く
			c.failover(addr, silent, alts)
		}
	}
}

// livenessTarget は ping を打つべき状況なら現在の接続・その dial 先・別 family の候補を
// 返す。打つべきでなければ conn == nil。
func (c *quicClient) livenessTarget() (conn *quic.Conn, addr string, alts []string) {
	if c.State() != transport.StateConnected || c.serverFeatures.Load()&FeatureLiveness == 0 {
		return nil, "", nil
	}
	c.migrateMu.Lock()
	conn, addr = c.conn, c.connAddr
	c.migrateMu.Unlock()
	alts = otherFamilyCandidates(c.currentCandidates(), addr)
	if conn == nil || len(alts) == 0 {
		return nil, "", nil
	}
	return conn, addr, alts
}

func (c *quicClient) currentCandidates() []string {
	c.candidatesMu.Lock()
	defer c.candidatesMu.Unlock()
	return c.candidates
}

func (c *quicClient) sendPing() {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	_ = writeFrame(c.ctrl, &ctrlMsg{Type: ctrlPing})
}

// failover は pong が silent の間途絶えた接続（addr）から、別 family の候補 alts への
// 乗り換えを試みる。失敗時は現在の接続・状態・バックオフに触れない（ファイル冒頭の方針）。
func (c *quicClient) failover(addr string, silent time.Duration, alts []string) {
	// recover（watchdog・打鍵起点）と排他。向こうが走っているならそちらに任せる。
	if !c.recovering.CompareAndSwap(false, true) {
		return
	}
	defer c.recovering.Store(false)

	start := time.Now()
	tlsConf, err := ClientTLS(c.k)
	if err != nil {
		return
	}
	// dial（最大 10s）は migrateMu の外で行う。接続は生きている扱いのままなので、
	// その間 Stats() 等を待たせない。
	winner, failures := dialParallel(c.ctx, alts, tlsConf)
	if winner == nil {
		c.logf("liveness: no response via %s for %v; failover to other family failed: %s",
			addr, silent.Truncate(time.Second), strings.Join(failures, "; "))
		return
	}

	c.migrateMu.Lock()
	defer c.migrateMu.Unlock()
	if c.State() != transport.StateConnected || c.connAddr != addr {
		// dial 中に他経路で復帰・差し替え済み
		_ = winner.conn.CloseWithError(0, "")
		_ = closeTransport(winner.tr)
		return
	}
	if err := c.adoptConn(winner); err != nil {
		c.logf("liveness: failover to %s failed: %v", winner.addr, err)
		return
	}
	d := c.recordRecovery(start)
	c.setState(transport.StateRecovering, fmt.Sprintf("failover: no response via %s for %v", addr, silent.Truncate(time.Second)))
	c.setState(transport.StateConnected, fmt.Sprintf("reconnected via %s (recovery %dms)", winner.addr, d.Milliseconds()))
}

// otherFamilyCandidates は candidates のうち current と別 family（v4/v6）のものを返す。
// current の family が判定できない（ホスト名等）場合は nil。
func otherFamilyCandidates(candidates []string, current string) []string {
	cur := addrFamily(current)
	if cur == "" {
		return nil
	}
	var alts []string
	for _, a := range candidates {
		if f := addrFamily(a); f != "" && f != cur {
			alts = append(alts, a)
		}
	}
	return alts
}

// addrFamily は "host:port" の host が IP リテラルなら "4"/"6" を、それ以外は "" を返す。
func addrFamily(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return ""
	case ip.To4() != nil:
		return "4"
	default:
		return "6"
	}
}
