package qtransport

// quicClient は transport.ClientTransport の QUIC 実装（server.go のプロトコル v1 に対応）。
// migration（ローミング）・offset 再同期は後続スライスで追加する。

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/quic-go/quic-go"
)

// quicConfig は client/server 共通の QUIC 設定。
// 長スリープでも接続を生かせるよう idle timeout を伸ばし、KeepAlive で経路を維持する。
func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:  60 * time.Second,
		KeepAlivePeriod: 15 * time.Second,
		// QUIC の Initial は anti-amplification のため最低 1200B にパディングされる。
		// 既定 1280B（IP で ~1308B）は、MTU が下がる経路（PPPoE / v6プラス等の
		// IPv4-over-IPv6 トンネル）＋ICMP フィルタの PMTUD ブラックホールで黙殺されうる。
		// RFC 下限の 1200B（IP で ~1228B）まで下げ、狭い MTU の経路でも通る確率を上げる。
		// これは初期値であって上限ではない: quic-go の DPLPMTUD（デフォルト有効）が
		// 確立後に probe で経路 MTU を検証しながら上限 1452B（Ethernet 1500 − IPv6/UDP
		// ヘッダ）まで自動で引き上げる。migration 時は 1200B に戻して新経路で再探索する。
		InitialPacketSize: 1200,
		// 小さい入力（打鍵）の投機的二重送信（DATAGRAM）用。トランスポートパラメータで
		// ネゴシエートされるため、非対応の旧相手とはストリームのみで動く。
		EnableDatagrams: true,
	}
}

// dialResult は並行 Dial の結果を保持する。
type dialResult struct {
	conn *quic.Conn
	tr   *quic.Transport
	addr string
	err  error // 失敗時のみ（conn == nil）。診断用に候補ごとの理由を運ぶ
}

// newLocalQUICTransport は新しいローカル UDP ソケット上に quic.Transport を作る。
// Start・migrate・dialParallel（候補ごと）で共通に使う。
func newLocalQUICTransport() (*quic.Transport, error) {
	uconn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	return &quic.Transport{Conn: uconn}, nil
}

// closeTransport は Transport と、それが内包する UDP ソケットを両方閉じる。
// tr は常に &quic.Transport{Conn: uconn} という struct literal で構築しており、
// quic-go 内部の createdConn フラグが立たないため、tr.Close() だけでは
// 下位の net.PacketConn（UDP ソケット）が閉じられず fd リークする。
func closeTransport(tr *quic.Transport) error {
	err := tr.Close()
	if cerr := tr.Conn.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

type quicClient struct {
	k            []byte
	serverAddr   string
	candidates   []string   // 再接続時に試す全候補（SetRoamingCandidates で設定）
	candidatesMu sync.Mutex // candidates の保護
	clientID     uint16
	sessionID    string

	tr       *quic.Transport
	conn     *quic.Conn
	ctrl     *quic.Stream     // control bidi（Hello/Resize 送信）
	in       *quic.SendStream // 入力 uni
	inConn   *quic.Conn       // in が属する conn（datagram 送信用。in と同時に差し替える）
	inOffset uint64           // in へ送った累計バイト数（datagram の offset。再接続でリセット）
	// inConn が DATAGRAM 対応か（接続確立時に一度だけ ConnectionState() を読んで
	// キャッシュする。トランスポートパラメータは接続ごとに不変なので打鍵ごとに
	// 問い合わせる必要がなく、また ConnectionState() は quic-go v0.60 内部の
	// switchToNewPath（migration の path 切替）と data race するため、migration が
	// 起きうるタイミングで呼んではいけない）
	inDatagramsOK bool
	ctrlMu        sync.Mutex
	inMu          sync.Mutex // in / inConn / inOffset / inDatagramsOK を保護
	migrateMu     sync.Mutex

	out    chan transport.OutputChunk
	ctx    context.Context
	cancel context.CancelFunc

	recovering atomic.Bool // recover() の多重起動防止
	// reconnect 連続失敗時の指数バックオフ。放置クライアント（-N トンネル等）が
	// サーバ消失後に dial（最大 10s）+ 候補リフレッシュの STUN 問い合わせを
	// 際限なく叩き続けるのを抑える。
	backoffMu    sync.Mutex
	backoffFails int       // 連続失敗回数
	backoffUntil time.Time // この時刻まで次の復帰試行を控える

	state          atomic.Int32
	lastOffset     atomic.Uint64 // 最後に配信した出力 offset（再同期 Hello・重複排除用）
	lastRecoveryMs atomic.Uint64 // 直近の復帰所要時間（ms）
	recoveryCount  atomic.Uint64 // ローミング/再接続の累計回数
	serverFeatures atomic.Uint64 // serverMeta で広告された機能ビット（FeatureTCPForward 等）

	// agent forwarding（-A）。Start() 前に SetAgentSockPath で設定する（agent.go）。
	// 空文字なら Hello の AgentForward は false（-A 未指定 or $SSH_AUTH_SOCK 不可）。
	agentSockPath string

	candidatesRefresher   func() []string // 全候補失敗時に呼ぶ候補再生成関数
	candidatesRefresherMu sync.Mutex

	cbMu              sync.Mutex
	onStateChange     func(old, new transport.ConnectionState, msg string)
	onServerMeta      func(buildID, buildTime string, instanceID []byte)
	onSessionNotFound func(string, int)
	onStatusMessage   func(string)
	onLogMessage      func(string)
}

// NewClient は serverAddr へ接続する QUIC クライアントトランスポートを作る。
// sessionID は共有 transport 上での routing 識別に使う（Hello に載せる）。
func NewClient(k []byte, serverAddr string, clientID uint16, sessionID string) (transport.ClientTransport, error) {
	c := &quicClient{
		k:          k,
		serverAddr: serverAddr,
		clientID:   clientID,
		sessionID:  sessionID,
		out:        make(chan transport.OutputChunk, 256),
	}
	c.state.Store(int32(transport.StateConnecting))
	return c, nil
}

// Start は接続を確立する。SetRoamingCandidates が Start 前に呼ばれていれば
// 全候補へ並行 Dial し、最初に確立したものを採用する（reconnect と同じ dialParallel。
// 以前は呼び出し側 cmd/tezzer が候補ごとに transport を丸ごと作って競争させる
// 二重実装を持っていたが、transport 内部に一本化した）。
// 候補未設定なら serverAddr へ単独 Dial する。
func (c *quicClient) Start(ctx context.Context) error {
	tlsConf, err := ClientTLS(c.k)
	if err != nil {
		return err
	}

	c.candidatesMu.Lock()
	candidates := c.candidates
	c.candidatesMu.Unlock()
	if len(candidates) == 0 {
		candidates = []string{c.serverAddr}
	}

	winner, failures := dialParallel(ctx, candidates, tlsConf)
	if winner == nil {
		return fmt.Errorf("all %d candidate(s) failed: %s", len(candidates), strings.Join(failures, "; "))
	}
	c.tr = winner.tr
	conn := winner.conn
	c.conn = conn

	// control bidi を開いて Hello を送る。
	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	c.ctrl = ctrl
	if err := writeFrame(ctrl, &ctrlMsg{Type: ctrlHello, ClientID: c.clientID, SessionID: c.sessionID, LastOffset: c.lastOffset.Load(), AgentForward: c.agentSockPath != ""}); err != nil {
		return err
	}

	// 入力 uni を開く。
	in, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	c.in = in
	c.inConn = conn
	// Dial 直後（この conn に migration が起きる前）に一度だけ読んでキャッシュする。
	c.inDatagramsOK = conn.ConnectionState().SupportsDatagrams.Remote

	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.setState(transport.StateConnected, "connected via "+winner.addr)
	go c.pumpOutput(conn)
	go c.readCtrl(ctrl)
	go c.acceptServerStreams(conn)
	go c.watchdog()
	return nil
}

// watchdog はスリープ復帰（実時間ジャンプ）を検出して migration を起動する。
// ノート PC のスリープ/復帰やネットワーク変更後、旧 socket の NAT バインディングが
// 失効していても新しい経路へ載せ替えてストリームを継続させる。
func (c *quicClient) watchdog() {
	const tick = 1 * time.Second
	// 旧 3s。WSL2 の VM アイドル制御による壁時計ジャンプ（~34s 周期で誤検知し、
	// 実機で recovery_count=1659/90h・接続は一度も死んでいないのに migrate/reconnect が
	// 誤発火し続けていたことを確認）を除外するため引き上げた。実際のノート PC スリープは
	// 分〜時間単位なので 15s にしても実害はない。
	const sleepThreshold = 15 * time.Second
	// jumped 誤検知の残存分への保険（上記の見積もりがずれていても被害を頭打ちにする）。
	// connDead() は対象外: 真に接続が死んだ場合はクールダウンなしで毎 tick リトライしてよい。
	const jumpRecoveryCooldown = 15 * time.Second
	// tick に対する超過がこの値を超えたらログする。sleepThreshold 未満のジャンプは
	// migration 不要だが、VM 一時停止（WSL2 のアイドル制御等）による体感数秒の
	// スタックの証拠になるため、黙って捨てずに記録して事後調査と突き合わせる。
	// wall と monotonic を並記する: wall のみ大きい = 時計ジャンプ（サスペンド・
	// VM 時刻再同期）、両方大きい = プロセス/VM がその間実行されていない。
	const tickGapLogThreshold = 500 * time.Millisecond
	last := time.Now()
	var lastJumpRecovery time.Time
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case now := <-t.C:
			// スリープ検出は wall-clock 差で行う。OS サスペンド中は monotonic clock が
			// 止まるため now.Sub(last)（monotonic）ではジャンプを検出できない。Round(0) で
			// monotonic を剥がし、wall 時計の差（サスペンド時間を含む）で判定する。
			jumped := now.Round(0).Sub(last.Round(0)) > sleepThreshold
			// tick gap の計測は ticker の値 now ではなく配送時点の実時刻で行う。
			// runtime は遅延した tick に「予定時刻」（Now() から遅延分を引いた値）を
			// 載せるため、monotonic が進むタイプの停止（SIGSTOP・VM pause 等）は
			// now からは見えない（SIGSTOP 2s の実験で delta ~1s のままを確認）。
			// suspend 検出（jumped）が now で機能するのは、サスペンドでは monotonic
			// 自体が止まり遅延補正がかからず wall ジャンプが値に残るため。
			nowReal := time.Now()
			wallDelta := nowReal.Round(0).Sub(last.Round(0))
			monoDelta := nowReal.Sub(last)
			last = nowReal
			if wallDelta-tick > tickGapLogThreshold || monoDelta-tick > tickGapLogThreshold {
				c.logf("watchdog: tick gap wall=%v mono=%v",
					wallDelta.Truncate(time.Millisecond), monoDelta.Truncate(time.Millisecond))
			}
			// recover は非同期で起動する。同期で呼ぶと高遅延環境では recover が数秒〜十数秒
			// ブロックし、その間 tick できず last が古くなって「ジャンプ」を誤検出 → 自己増殖
			// ループになる（実機で Recoveries=92 を観測）。多重起動は recovering フラグが防ぐ。
			switch {
			case c.State() == transport.StateRecovering:
				// 復帰未完了なら毎 tick リトライ（ネットワーク復旧待ち）。
				go c.recover("retry recovery")
			case jumped:
				if time.Since(lastJumpRecovery) < jumpRecoveryCooldown {
					continue
				}
				lastJumpRecovery = time.Now()
				// スリープ復帰はネットワーク環境が変わった可能性が高いので、
				// バックオフをリセットして即試行する。
				c.resetBackoff()
				go c.recover("sleep wake detected")
			case c.connDead():
				// サスペンドを伴わない経路断・接続死も拾う。
				go c.recover("connection lost")
			}
		}
	}
}

// connDead は現在の QUIC 接続が終了している（idle timeout 等で死んだ）かを返す。
func (c *quicClient) connDead() bool {
	c.migrateMu.Lock()
	conn := c.conn
	c.migrateMu.Unlock()
	return conn != nil && conn.Context().Err() != nil
}

// reconnect 連続失敗時のバックオフ。初回失敗後 reconnectBackoffBase、以後倍々で
// reconnectBackoffMax まで伸びる（実効間隔にはさらに試行自体の所要時間 =
// dialParallel の最大 10s が加算される）。
const (
	reconnectBackoffBase = 1 * time.Second
	reconnectBackoffMax  = 60 * time.Second
)

// inBackoff は次の復帰試行を控えるべき間 true を返す。
func (c *quicClient) inBackoff() bool {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	return time.Now().Before(c.backoffUntil)
}

// bumpBackoff は reconnect の連続失敗を記録し、次試行までの待ちを倍々で伸ばす。
func (c *quicClient) bumpBackoff() time.Duration {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	d := reconnectBackoffBase << uint(c.backoffFails)
	if d <= 0 || d > reconnectBackoffMax {
		d = reconnectBackoffMax
	}
	if c.backoffFails < 30 { // シフトのオーバーフロー防止（上限到達後は増やす意味がない）
		c.backoffFails++
	}
	c.backoffUntil = time.Now().Add(d)
	return d
}

// resetBackoff は連続失敗の記録をクリアし、次の復帰試行を即時可能にする。
// 成功時と、環境変化が濃厚な外部イベント（スリープ復帰・ユーザー打鍵）で呼ぶ。
func (c *quicClient) resetBackoff() {
	c.backoffMu.Lock()
	c.backoffFails = 0
	c.backoffUntil = time.Time{}
	c.backoffMu.Unlock()
}

// recover は経路復帰を試みる。まず能動 migration（接続生存時）、失敗したら
// full reconnect（接続死＝idle timeout 超の長スリープ時）にフォールバックする。
func (c *quicClient) recover(reason string) {
	// 多重起動防止（打鍵連打 ＋ watchdog tick が同時に来ても 1 本だけ走らせる）。
	if !c.recovering.CompareAndSwap(false, true) {
		return
	}
	defer c.recovering.Store(false)

	// 連続失敗中はバックオフ（次試行時刻まで何もしない）。watchdog は毎 tick 呼ぶが
	// ここで弾く。スリープ復帰・打鍵起点の復帰は resetBackoff 済みで来るため、
	// ユーザーが現に使おうとしている場面で待たされることはない。
	if c.inBackoff() {
		return
	}

	// 接続が生きていれば軽い能動 migration を試す（同一 NW のスリープ/NAT rebind）。
	// 既に死んでいる場合は migrate（AddPath/Probe）が無駄に Probe タイムアウトまで
	// 待つので、直接 reconnect する。
	if !c.connDead() {
		if err := c.migrate(reason); err == nil {
			c.resetBackoff()
			return
		}
	}
	if err := c.reconnect(reason); err != nil {
		delay := c.bumpBackoff()
		c.logf("reconnect failed (%v); next retry in %v", err, delay)
	} else {
		c.resetBackoff()
	}
}

// reconnect は接続が死んだ後（長スリープ > idle timeout）に新しい QUIC 接続を張り直す。
// 保持している lastOffset を Hello に載せるため、サーバは OnResyncNeeded で欠損分を再送する。
// 全候補に並行 Dial し、最初に確立したものを採用する。
func (c *quicClient) reconnect(reason string) error {
	c.migrateMu.Lock()
	defer c.migrateMu.Unlock()

	start := time.Now()
	c.setState(transport.StateRecovering, "reconnecting: "+reason)

	tlsConf, err := ClientTLS(c.k)
	if err != nil {
		return err
	}

	c.candidatesMu.Lock()
	candidates := c.candidates
	c.candidatesMu.Unlock()
	if len(candidates) == 0 {
		candidates = []string{c.serverAddr}
	}

	winner, failures := dialParallel(c.ctx, candidates, tlsConf)

	// 全候補失敗時: 候補リフレッシュ関数が登録されていれば再探索して再試行する。
	// スリープ復帰後にネットワーク環境が変わった場合（リモート↔LAN）に対応。
	if winner == nil {
		c.candidatesRefresherMu.Lock()
		refresher := c.candidatesRefresher
		c.candidatesRefresherMu.Unlock()

		if refresher != nil {
			freshCandidates := refresher()
			if len(freshCandidates) > 0 && !candidatesEqual(freshCandidates, candidates) {
				c.setState(transport.StateRecovering, "reconnecting: refreshed candidates")
				winner, failures = dialParallel(c.ctx, freshCandidates, tlsConf)
				if winner != nil {
					// 次回 reconnect からは新しい候補を使う
					c.candidatesMu.Lock()
					c.candidates = freshCandidates
					c.candidatesMu.Unlock()
				}
			}
		}
	}

	if winner == nil {
		return fmt.Errorf("reconnect: all %d candidate(s) failed: %s", len(candidates), strings.Join(failures, "; "))
	}

	// winner への接続でストリームを構築する。
	// dialCtx はキャンセル済みなので新たなコンテキストを用いる。
	setupCtx, setupCancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer setupCancel()

	ctrl, err := winner.conn.OpenStreamSync(setupCtx)
	if err != nil {
		_ = winner.conn.CloseWithError(0, "")
		_ = closeTransport(winner.tr)
		return err
	}
	if err := writeFrame(ctrl, &ctrlMsg{Type: ctrlHello, ClientID: c.clientID, SessionID: c.sessionID, LastOffset: c.lastOffset.Load(), AgentForward: c.agentSockPath != ""}); err != nil {
		_ = winner.conn.CloseWithError(0, "")
		_ = closeTransport(winner.tr)
		return err
	}
	in, err := winner.conn.OpenUniStreamSync(setupCtx)
	if err != nil {
		_ = winner.conn.CloseWithError(0, "")
		_ = closeTransport(winner.tr)
		return err
	}

	// 旧 conn/transport を退役（旧 pumpOutput は stream エラーで自然終了）。
	if c.conn != nil {
		_ = c.conn.CloseWithError(0, "reconnect")
	}
	c.retireTransport(c.tr)
	c.conn = winner.conn
	c.tr = winner.tr
	c.ctrlMu.Lock()
	c.ctrl = ctrl
	c.ctrlMu.Unlock()
	c.inMu.Lock()
	c.in = in
	c.inConn = winner.conn
	// Dial 直後（この conn に migration が起きる前）に一度だけ読んでキャッシュする。
	c.inDatagramsOK = winner.conn.ConnectionState().SupportsDatagrams.Remote
	c.inOffset = 0 // 新しい入力ストリームなので offset を振り直す（server 側 dedup も接続単位）
	c.inMu.Unlock()

	c.recordRecovery(start)
	c.setState(transport.StateConnected, fmt.Sprintf("reconnected via %s", winner.addr))
	go c.pumpOutput(winner.conn)
	go c.readCtrl(ctrl)
	go c.acceptServerStreams(winner.conn)
	return nil
}

// migrate は新しいローカル socket へ能動 migration する（spike B/C の AddPath/Probe/Switch）。
// ストリーム状態は保持されるため、入力/出力/制御ストリームの再構築は不要。
func (c *quicClient) migrate(reason string) error {
	c.migrateMu.Lock()
	defer c.migrateMu.Unlock()

	start := time.Now()
	c.setState(transport.StateRecovering, "roaming: "+reason)

	tr, err := newLocalQUICTransport()
	if err != nil {
		return err
	}

	path, err := c.conn.AddPath(tr)
	if err != nil {
		_ = closeTransport(tr)
		return err
	}
	// Probe タイムアウトは短め（高遅延でも空振りを長引かせず reconnect へ早く落とす）。
	pctx, cancel := context.WithTimeout(c.ctx, 4*time.Second)
	defer cancel()
	if err := path.Probe(pctx); err != nil {
		_ = closeTransport(tr)
		return err
	}
	if err := path.Switch(); err != nil {
		_ = closeTransport(tr)
		return err
	}

	// 旧 transport は即閉じると競合しうるため、猶予をおいて閉じる。
	c.retireTransport(c.tr)
	c.tr = tr
	c.recordRecovery(start)
	c.setState(transport.StateConnected, "roaming: migrated")
	return nil
}

// transportRetireGrace は退役した transport（旧 UDP ソケット）を閉じるまでの猶予。
// Switch/reconnect 直後に即閉じすると quic-go 内部のパス処理（旧経路への
// CONNECTION_CLOSE 送出・再送等）と競合しうるため少し待つ。
const transportRetireGrace = 30 * time.Second

// retireTransport は退役した transport を猶予をおいて（または Close 時に即）閉じる。
// 従来は Close() までまとめて保持していたが、復帰のたびに UDP ソケット fd が溜まり
// リークしていた（WSL2 の watchdog 誤検知事案では 90h で recovery 1659 回 = fd 1659 個）。
func (c *quicClient) retireTransport(tr *quic.Transport) {
	go func() {
		select {
		case <-time.After(transportRetireGrace):
		case <-c.ctx.Done():
		}
		_ = closeTransport(tr)
	}()
}

// recordRecovery は復帰所要時間と回数を記録する。
func (c *quicClient) recordRecovery(start time.Time) {
	c.lastRecoveryMs.Store(uint64(time.Since(start).Milliseconds()))
	c.recoveryCount.Add(1)
}

// pumpOutput は指定 conn のサーバ出力 uni ストリームを受けて Output() へ流す。
// reconnect で conn が差し替わると古い pump は stream エラーで終了し、新しい conn 用の
// pump が起動される。
func (c *quicClient) pumpOutput(conn *quic.Conn) {
	rs, err := conn.AcceptUniStream(c.ctx)
	if err != nil {
		return
	}
	for {
		offset, data, err := readOutputFrame(rs)
		if err != nil {
			return
		}
		// 重複排除: 既に配信済みの offset 以下は捨てる（再同期での再送を弾く）。
		if offset <= c.lastOffset.Load() {
			continue
		}
		c.lastOffset.Store(offset)
		select {
		case c.out <- transport.OutputChunk{Offset: offset, Data: data}:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *quicClient) SendInput(data []byte) error {
	// offset とストリーム書き込みの整合を保つため、Write まで inMu 下で行う
	// （実際の呼び出し元は handleStdin の単一 goroutine なので競合はほぼない）。
	c.inMu.Lock()
	in := c.in
	conn := c.inConn
	datagramsOK := c.inDatagramsOK
	offset := c.inOffset
	_, err := in.Write(data)
	if err == nil {
		c.inOffset += uint64(len(data))
	}
	c.inMu.Unlock()
	if err != nil {
		// 接続死（スリープ復帰直後など）を打鍵で踏んだら即復帰を起動する
		// （watchdog の次 tick を待たず体感を改善）。多重起動は migrateMu で直列化。
		// ユーザー操作起点なのでバックオフもリセットして待たせない。
		go func() {
			c.resetBackoff()
			c.recover("input send failed")
		}()
		return err
	}
	// 小さい入力（打鍵）は DATAGRAM でも投機送信し、ロス時のストリーム再送待ち
	// （head-of-line blocking）を回避する。ベストエフォートなので失敗は無視。
	// server は offset で重複排除するため、どちらが先に届いても一度だけ適用される。
	if len(data) <= maxDupInputSize && conn != nil && datagramsOK {
		_ = conn.SendDatagram(encodeInputDatagram(offset, data))
	}
	return nil
}

func (c *quicClient) SendResize(cols, rows int) error {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	return writeFrame(c.ctrl, &ctrlMsg{Type: ctrlResize, Cols: cols, Rows: rows})
}

func (c *quicClient) Output() <-chan transport.OutputChunk { return c.out }

// SeedResyncOffset は接続前に「既に他経路（UDS）で受信・描画済みの出力オフセット」を
// 設定する（transport.ClientTransport には含めず、呼び出し側が型アサーションで使う）。
// Hello の LastOffset に反映され、サーバは再同期でこのオフセットより後のみを送るため、
// attach 時に UDS で描画済みのバックログを QUIC が二重転送しなくなる。
// Start() 前に一度だけ呼ぶこと。
func (c *quicClient) SeedResyncOffset(offset uint64) {
	c.lastOffset.Store(offset)
}

func (c *quicClient) State() transport.ConnectionState {
	return transport.ConnectionState(c.state.Load())
}

func (c *quicClient) setState(s transport.ConnectionState, msg string) {
	old := transport.ConnectionState(c.state.Swap(int32(s)))
	if old == s {
		return
	}
	c.cbMu.Lock()
	fn := c.onStateChange
	c.cbMu.Unlock()
	if fn != nil {
		fn(old, s, msg)
	}
}

func (c *quicClient) OnStateChange(fn func(old, new transport.ConnectionState, msg string)) {
	c.cbMu.Lock()
	c.onStateChange = fn
	c.cbMu.Unlock()
}

// server→client の制御通知（control ストリーム経由。readCtrl が配送する）。
func (c *quicClient) OnStatusMessage(fn func(string)) {
	c.cbMu.Lock()
	c.onStatusMessage = fn
	c.cbMu.Unlock()
}
func (c *quicClient) OnLogMessage(fn func(string)) {
	c.cbMu.Lock()
	c.onLogMessage = fn
	c.cbMu.Unlock()
}

// logf はクライアント側で生成した診断メッセージを OnLogMessage 経由で上位
// （クライアントログファイル・stats の通知リスト）へ送る。コールバック未登録
// （Start 直後〜登録前）の間は捨てる。
func (c *quicClient) logf(format string, args ...any) {
	c.cbMu.Lock()
	onLog := c.onLogMessage
	c.cbMu.Unlock()
	if onLog != nil {
		onLog(fmt.Sprintf(format, args...))
	}
}
func (c *quicClient) OnServerMeta(fn func(buildID, buildTime string, instanceID []byte)) {
	c.cbMu.Lock()
	c.onServerMeta = fn
	c.cbMu.Unlock()
}
func (c *quicClient) OnSessionNotFound(fn func(string, int)) {
	c.cbMu.Lock()
	c.onSessionNotFound = fn
	c.cbMu.Unlock()
}
func (c *quicClient) SetRoamingCandidates(addrs []string) {
	c.candidatesMu.Lock()
	c.candidates = addrs
	c.candidatesMu.Unlock()
}

func (c *quicClient) SetCandidatesRefresher(fn func() []string) {
	c.candidatesRefresherMu.Lock()
	c.candidatesRefresher = fn
	c.candidatesRefresherMu.Unlock()
}

// dialParallel は candidates に並行 Dial し、最初に確立した接続と、失敗した候補の
// 理由（"addr: err" 形式。診断用）を返す。全候補失敗時は winner が nil。
// ctx にタイムアウトがなくても内部で 10s を上限にする。
// 勝者確定後にキャンセルされた残候補の失敗も failures に混ざるが、呼び出し側が
// failures を使うのは winner == nil のときだけなので実害はない。
func dialParallel(ctx context.Context, candidates []string, tlsConf *tls.Config) (*dialResult, []string) {
	ch := make(chan dialResult, len(candidates))
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)

	var wg sync.WaitGroup
	for _, addr := range candidates {
		addr := addr
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr, err := newLocalQUICTransport()
			if err != nil {
				ch <- dialResult{addr: addr, err: err}
				return
			}
			raddr, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				_ = closeTransport(tr)
				ch <- dialResult{addr: addr, err: err}
				return
			}
			conn, err := tr.Dial(dialCtx, raddr, tlsConf, quicConfig())
			if err != nil {
				_ = closeTransport(tr)
				ch <- dialResult{addr: addr, err: err}
				return
			}
			ch <- dialResult{conn: conn, tr: tr, addr: addr}
		}()
	}
	go func() {
		wg.Wait()
		close(ch)
		dialCancel()
	}()

	var winner *dialResult
	var failures []string
	for r := range ch {
		if r.err != nil {
			failures = append(failures, r.addr+": "+r.err.Error())
			continue
		}
		if winner == nil {
			rCopy := r
			winner = &rCopy
			dialCancel()
		} else {
			_ = r.conn.CloseWithError(0, "")
			_ = closeTransport(r.tr)
		}
	}
	return winner, failures
}

// candidatesEqual は2つの候補リストが同じかを返す（順序も含む）。
func candidatesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readCtrl は control bidi ストリームから server→client の制御フレームを読み、配送する。
// reconnect で control ストリームが張り替わるたびに新しいストリームで起動される。
func (c *quicClient) readCtrl(ctrl *quic.Stream) {
	for {
		m, err := readFrame(ctrl)
		if err != nil {
			return
		}
		c.cbMu.Lock()
		onMeta := c.onServerMeta
		onGone := c.onSessionNotFound
		onStatus := c.onStatusMessage
		onLog := c.onLogMessage
		c.cbMu.Unlock()
		switch m.Type {
		case ctrlServerMeta:
			c.serverFeatures.Store(m.Features)
			if onMeta != nil {
				onMeta(m.BuildID, m.BuildTime, m.InstanceID)
			}
		case ctrlSessionGone:
			if onGone != nil {
				exitCode := -1
				if m.ExitCode != nil {
					exitCode = int(*m.ExitCode)
				}
				onGone(m.Msg, exitCode)
			}
		case ctrlStatus:
			if onStatus != nil {
				onStatus(m.Msg)
			}
		case ctrlLog:
			if onLog != nil {
				onLog(m.Msg)
			}
		}
	}
}

func (c *quicClient) Stats() transport.Stats {
	st := transport.Stats{
		LastRecoveryMs: float64(c.lastRecoveryMs.Load()),
		RecoveryCount:  c.recoveryCount.Load(),
	}
	c.migrateMu.Lock()
	conn := c.conn
	c.migrateMu.Unlock()
	if conn != nil {
		cs := conn.ConnectionStats()
		st.RTT = float64(cs.SmoothedRTT.Microseconds()) / 1000.0
		st.BytesSent = cs.BytesSent
		st.BytesReceived = cs.BytesReceived
		if cs.PacketsSent > 0 {
			st.LossRate = float64(cs.PacketsLost) / float64(cs.PacketsSent)
		}
	}
	return st
}

func (c *quicClient) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.setState(transport.StateClosed, "closed")
	if c.conn != nil {
		_ = c.conn.CloseWithError(0, "done")
	}
	c.migrateMu.Lock()
	if c.tr != nil {
		_ = closeTransport(c.tr)
	}
	// 退役済み transport は retireTransport の goroutine が ctx.Done（上の cancel）を
	// 見て閉じる。
	c.migrateMu.Unlock()
	return nil
}

var (
	_ transport.ClientTransport    = (*quicClient)(nil)
	_ transport.ResyncSeeder       = (*quicClient)(nil)
	_ transport.AgentForwardClient = (*quicClient)(nil)
)
