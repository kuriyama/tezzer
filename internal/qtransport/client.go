package qtransport

// client.go: QUIC トランスポートのクライアント側（server.go のプロトコル v1 に対応）。

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// Client is the client side of the QUIC terminal transport. It survives
// network changes by connection migration and, when the connection is dead,
// by reconnecting and resynchronizing output from the last received offset.
type Client struct {
	k            []byte
	candidates   []string   // 接続・再接続時に試す全候補（ClientConfig.Candidates、以後 SetRoamingCandidates で更新）
	candidatesMu sync.Mutex // candidates の保護
	clientID     uint16
	sessionID    string

	// pathTransports は現在の conn が migration 前に使っていた（または probe に失敗した）
	// transport。quic-go は AddPath した transport にも conn を登録し、transport を閉じると
	// 登録中の conn ごと destroy するため、conn を差し替えるまで閉じられない。migrateMu で保護
	pathTransports []*quic.Transport

	tr *quic.Transport // conn が使う transport（migrateMu で保護）
	// conn は現在の QUIC 接続。差し替え（初回接続と reconnect / failover の adoptConn）は
	// migrateMu の下で行い、読むだけの側（Stats・死活判定・OpenForward 等）はロックを
	// 取らずに Load する。migrateMu は reconnect の dial・setup（最大 20 秒程度）や
	// migration の probe の間握られたままなので、ロックで読むと Ctrl-^ i の表示
	// （stdin を処理する goroutine から呼ばれる）ごと止まってしまう。
	conn atomic.Pointer[quic.Conn]
	// remoteAddr は conn の dial 先候補（"host:port"）。migration はローカル側の経路だけを
	// 変えるので、変わるのは初回接続と reconnect のとき（conn と同じく migrateMu 下で書き、
	// ロックなしで読む）。
	remoteAddr atomic.Pointer[string]
	ctrl       *quic.Stream     // control bidi（Hello/Resize 送信。ctrlMu で保護）
	in         *quic.SendStream // 入力 uni
	inConn     *quic.Conn       // in が属する conn（datagram 送信用。in と同時に差し替える）
	inOffset   uint64           // in へ送った累計バイト数（datagram の offset。再接続でリセット）
	// inConn が DATAGRAM 対応か（接続確立時に一度だけ ConnectionState() を読んで
	// キャッシュする。トランスポートパラメータは接続ごとに不変なので打鍵ごとに
	// 問い合わせる必要がなく、また ConnectionState() は quic-go v0.60 内部の
	// switchToNewPath（migration の path 切替）と data race するため、migration が
	// 起きうるタイミングで呼んではいけない）
	inDatagramsOK bool
	// inStalled は前回の入力 Write が inputWriteTimeout で打ち切られた（経路が詰まっている）。
	// 立っている間は短い期限で試し、成功したら下ろす（入力ストリームの差し替えでも下ろす）。
	inStalled bool
	lost      lostInput // 届かなかった入力の記録（lostinput.go）
	ctrlMu    sync.Mutex
	inMu      sync.Mutex // in / inConn / inOffset / inDatagramsOK / inStalled / lost を保護
	migrateMu sync.Mutex

	out    chan OutputChunk
	ctx    context.Context
	cancel context.CancelFunc

	recovering atomic.Bool // recover() の多重起動防止
	// reconnect 連続失敗時の指数バックオフ。放置クライアント（-N トンネル等）が
	// サーバ消失後に dial（最大 10s）+ 候補リフレッシュの STUN 問い合わせを
	// 際限なく叩き続けるのを抑える。
	backoffMu    sync.Mutex
	backoffFails int       // 連続失敗回数
	backoffUntil time.Time // この時刻まで次の復帰試行を控える
	// 打鍵起点の resetBackoff を最後に行った時刻（resetBackoffByInput の間引き用。backoffMu で保護）
	lastInputBackoffReset time.Time

	state          atomic.Int32
	lastOffset     atomic.Uint64 // 最後に配信した出力 offset（再同期 Hello・重複排除用）
	lastRecoveryMs atomic.Uint64 // 直近の復帰所要時間（ms）
	recoveryCount  atomic.Uint64 // ローミング/再接続の累計回数
	serverFeatures atomic.Uint64 // serverMeta で広告された機能ビット（FeatureTCPForward 等）
	lastPongNano   atomic.Int64  // 最後に pong を受けた時刻（liveness.go）

	// 以下は ClientConfig から NewClient で設定し、以後は読み取りのみ（ロック不要）。

	// agent forwarding（-A）（agent.go）。
	// 空文字なら Hello の AgentForward は false（-A 未指定 or $SSH_AUTH_SOCK 不可）。
	agentSockPath string

	// -R（rforward.go）。
	deviceID    string
	rfwdTargets map[uint16]string // サーバ側 listen ポート → クライアント側 dial 先
	rfwdActive  atomic.Int32

	candidatesRefresher func() []string // 全候補失敗時に呼ぶ候補再生成関数

	onStateChange     func(old, new ConnectionState, msg string)
	onServerMeta      func(buildID, buildTime string, instanceID []byte)
	onSessionNotFound func(string, int, uint64)
	onInputLost       func(int, []byte)
	onStatusMessage   func(string)
	onLogMessage      func(string)
}

// ClientConfig configures a QUIC client transport. Everything here is fixed
// at construction; only the roaming candidates can be replaced later
// (SetRoamingCandidates).
type ClientConfig struct {
	// Key is the shared key K used for mTLS pinning.
	Key []byte
	// Candidates are the server addresses ("host:port") to dial in parallel,
	// in priority order. Must not be empty. The slice is copied.
	Candidates []string
	// ClientID is the client-chosen identifier carried in the Hello.
	ClientID uint16
	// SessionID identifies the session for routing on a shared transport.
	SessionID string
	// ResyncOffset is the output offset already received and rendered via
	// another path (the Unix socket). It is sent as the Hello's LastOffset so
	// the server does not transfer that backlog twice.
	ResyncOffset uint64
	// AgentSockPath is the local SSH agent socket to relay (-A). Empty
	// disables agent forwarding.
	AgentSockPath string
	// DeviceID and RemoteForwards configure remote forwarding (-R):
	// RemoteForwards maps a server-side listen port to the client-side
	// target to dial. The map is copied.
	DeviceID       string
	RemoteForwards map[uint16]string
	// RefreshCandidates, if set, is called when dialing every candidate
	// fails during a reconnect, to obtain fresh candidates.
	RefreshCandidates func() []string

	// Callbacks. Any of them may be nil.

	// OnStateChange is called on every connection state transition.
	OnStateChange func(old, new ConnectionState, msg string)
	// OnStatusMessage receives server status notifications.
	OnStatusMessage func(msg string)
	// OnLogMessage receives diagnostic messages from the server and the
	// client transport itself.
	OnLogMessage func(msg string)
	// OnServerMeta receives the server's build and instance information.
	OnServerMeta func(buildID, buildTime string, instanceID []byte)
	// OnSessionNotFound is called when the server reports that the session
	// no longer exists. exitCode is the session process's exit status, or -1
	// when unknown. finalOffset is the offset of the session's last output
	// (0 = unknown); the notification can overtake that output, which arrives
	// on another stream.
	OnSessionNotFound func(msg string, exitCode int, finalOffset uint64)
	// OnInputLost is called after a recovery when input typed before or during
	// the outage did not reach the session (it is not re-sent). total is the
	// number of bytes; head holds the first bytes of that input, which may
	// include passwords typed at a prompt.
	OnInputLost func(total int, head []byte)
}

// NewClient creates a QUIC client transport. Call Start to connect.
func NewClient(cfg ClientConfig) *Client {
	c := &Client{
		k:                   cfg.Key,
		candidates:          slices.Clone(cfg.Candidates),
		clientID:            cfg.ClientID,
		sessionID:           cfg.SessionID,
		agentSockPath:       cfg.AgentSockPath,
		deviceID:            cfg.DeviceID,
		rfwdTargets:         maps.Clone(cfg.RemoteForwards),
		candidatesRefresher: cfg.RefreshCandidates,
		onStateChange:       cfg.OnStateChange,
		onServerMeta:        cfg.OnServerMeta,
		onSessionNotFound:   cfg.OnSessionNotFound,
		onInputLost:         cfg.OnInputLost,
		onStatusMessage:     cfg.OnStatusMessage,
		onLogMessage:        cfg.OnLogMessage,
		out:                 make(chan OutputChunk, 256),
	}
	c.lastOffset.Store(cfg.ResyncOffset)
	c.state.Store(int32(StateConnecting))
	return c
}

// Start establishes the connection, dialing every candidate in parallel and
// adopting the first that succeeds (the same way as reconnects).
func (c *Client) Start(ctx context.Context) error {
	tlsConf, err := ClientTLS(c.k)
	if err != nil {
		return err
	}

	c.candidatesMu.Lock()
	candidates := c.candidates
	c.candidatesMu.Unlock()
	if len(candidates) == 0 {
		return errors.New("no candidate addresses")
	}

	winner, failures := dialParallel(ctx, candidates, tlsConf)
	if winner == nil {
		return fmt.Errorf("all %d candidate(s) failed: %s", len(candidates), strings.Join(failures, "; "))
	}

	// adoptConn が立てる goroutine（pumpOutput・退役 transport の掃除など）が c.ctx を見るので先に作る。
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.migrateMu.Lock()
	err = c.adoptConn(ctx, winner)
	c.migrateMu.Unlock()
	if err != nil {
		return err
	}
	c.setState(StateConnected, "connected via "+winner.addr)
	go c.watchdog()
	go c.livenessLoop()
	return nil
}

// watchdog はスリープ復帰（実時間ジャンプ）を検出して migration を起動する。
// ノート PC のスリープ/復帰やネットワーク変更後、旧 socket の NAT バインディングが
// 失効していても新しい経路へ載せ替えてストリームを継続させる。
func (c *Client) watchdog() {
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
			slept := now.Round(0).Sub(last.Round(0))
			jumped := slept > sleepThreshold
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
			case c.State() == StateRecovering:
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
				if serverLikelyTimedOut(slept) {
					// サーバー側は idle timeout で接続を捨てている見込みが高く、migration の
					// Probe は応答なしで 4 秒待つだけになるので、直接 reconnect する。
					go c.recoverWith("sleep wake detected", false)
				} else {
					go c.recover("sleep wake detected")
				}
			case c.connDead():
				// サスペンドを伴わない経路断・接続死も拾う。
				go c.recover("connection lost")
			}
		}
	}
}

// connDead は現在の QUIC 接続が終了している（idle timeout 等で死んだ）かを返す。
func (c *Client) connDead() bool {
	conn := c.conn.Load()
	return conn != nil && conn.Context().Err() != nil
}

// reconnect 連続失敗時のバックオフ。初回失敗後 reconnectBackoffBase、以後倍々で
// reconnectBackoffMax まで伸びる（実効間隔にはさらに試行自体の所要時間 =
// dialParallel の最大 10s が加算される）。
const (
	reconnectBackoffBase = 1 * time.Second
	reconnectBackoffMax  = 60 * time.Second
	// 打鍵起点でバックオフをリセットする最小間隔。接続断中はマウストラッキング等で
	// 端末が自動生成する ESC シーケンスが送信失敗を連発するため、毎回リセットすると
	// バックオフが効かず reconnect を間断なく試し続けてしまう。
	inputBackoffResetInterval = 5 * time.Second
)

// inBackoff は次の復帰試行を控えるべき間 true を返す。
func (c *Client) inBackoff() bool {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	return time.Now().Before(c.backoffUntil)
}

// bumpBackoff は reconnect の連続失敗を記録し、次試行までの待ちを倍々で伸ばす。
func (c *Client) bumpBackoff() time.Duration {
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
func (c *Client) resetBackoff() {
	c.backoffMu.Lock()
	c.backoffFails = 0
	c.backoffUntil = time.Time{}
	c.backoffMu.Unlock()
}

// resetBackoffByInput は打鍵起点のバックオフリセットを inputBackoffResetInterval に
// 1 回までに間引く（ユーザーが使おうとしている場面で即復帰させる意図は保ちつつ、
// 自動生成入力による reconnect 連打を防ぐ）。
func (c *Client) resetBackoffByInput() {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	now := time.Now()
	if !c.lastInputBackoffReset.IsZero() && now.Sub(c.lastInputBackoffReset) < inputBackoffResetInterval {
		return
	}
	c.lastInputBackoffReset = now
	c.backoffFails = 0
	c.backoffUntil = time.Time{}
}

// serverLikelyTimedOut は、クライアントが slept の間止まっていた（スリープ等）場合に、
// サーバー側で接続が idle timeout 済みと見込めるかを返す。サーバーが最後に受信したのは
// 停止の最大 KeepAlivePeriod 前なので、slept が MaxIdleTimeout − KeepAlivePeriod 以上なら
// サーバーから見た無音は idle timeout に達しうる。クライアント側は停止中に monotonic
// clock が進まず自分の接続を生きていると見なすため、connDead() では判定できない。
func serverLikelyTimedOut(slept time.Duration) bool {
	cfg := quicConfig()
	return slept >= cfg.MaxIdleTimeout-cfg.KeepAlivePeriod
}

// recover は経路復帰を試みる。まず能動 migration（接続生存時）、失敗したら
// full reconnect（接続死＝idle timeout 超の長スリープ時）にフォールバックする。
func (c *Client) recover(reason string) { c.recoverWith(reason, true) }

// recoverWith は recover の本体。tryMigrate=false なら migration を試さず直接 reconnect する。
func (c *Client) recoverWith(reason string, tryMigrate bool) {
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
	if tryMigrate && !c.connDead() {
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
func (c *Client) reconnect(reason string) error {
	c.migrateMu.Lock()
	defer c.migrateMu.Unlock()

	start := time.Now()
	c.setState(StateRecovering, "reconnecting: "+reason)

	tlsConf, err := ClientTLS(c.k)
	if err != nil {
		return err
	}

	c.candidatesMu.Lock()
	candidates := c.candidates
	c.candidatesMu.Unlock()

	winner, failures := dialParallel(c.ctx, candidates, tlsConf)
	if winner == nil && c.ctx.Err() != nil {
		// Close 済み。失敗は cancel によるものなので候補を取り直さない（STUN は cancel を
		// 見ずに最大 5 秒 × family 待つので、migrateMu を待つ Close が終了を止めてしまう）。
		return c.ctx.Err()
	}

	// 全候補失敗時: 候補リフレッシュ関数が登録されていれば再探索して再試行する。
	// スリープ復帰後にネットワーク環境が変わった場合（リモート↔LAN）に対応。
	if winner == nil {
		if c.candidatesRefresher != nil {
			freshCandidates := c.candidatesRefresher()
			if len(freshCandidates) > 0 && !slices.Equal(freshCandidates, candidates) {
				c.setState(StateRecovering, "reconnecting: refreshed candidates")
				winner, failures = dialParallel(c.ctx, freshCandidates, tlsConf)
				if winner != nil {
					// 次回 reconnect からは新しい候補を使う
					c.candidatesMu.Lock()
					c.candidates = slices.Clone(freshCandidates)
					c.candidatesMu.Unlock()
				}
			}
		}
	}

	if winner == nil {
		return fmt.Errorf("reconnect: all %d candidate(s) failed: %s", len(candidates), strings.Join(failures, "; "))
	}

	if err := c.adoptConn(c.ctx, winner); err != nil {
		return err
	}
	d := c.recordRecovery(start)
	c.setState(StateConnected, fmt.Sprintf("reconnected via %s (recovery %dms)", winner.addr, d.Milliseconds()))
	return nil
}

// adoptConn は dial 済みの winner 上にストリーム（control/入力）を構築して Hello を送り、
// 現在の接続と差し替える。migrateMu を保持した状態で呼ぶこと。失敗時は winner を閉じ、
// 現在の接続には触れない。初回接続（Start）・reconnect・liveness の別 family 切替
// （liveness.go）で共用する。ctx はストリーム構築の親（初回は呼び出し側の期限、
// 復帰は c.ctx）で、構築自体にも 10 秒の上限を設ける。
func (c *Client) adoptConn(ctx context.Context, winner *dialResult) error {
	setupCtx, setupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer setupCancel()

	ctrl, err := winner.conn.OpenStreamSync(setupCtx)
	if err != nil {
		_ = winner.conn.CloseWithError(0, "")
		_ = closeTransport(winner.tr)
		return err
	}
	if err := writeFrame(ctrl, c.helloMsg()); err != nil {
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

	// 旧 conn/transport を退役（旧 pumpOutput は stream エラーで自然終了）。初回接続には無い。
	if old := c.conn.Load(); old != nil {
		_ = old.CloseWithError(0, "reconnect")
	}
	if c.tr != nil {
		c.retireTransport(c.tr)
	}
	for _, tr := range c.pathTransports {
		c.retireTransport(tr)
	}
	c.pathTransports = nil
	c.conn.Store(winner.conn)
	c.remoteAddr.Store(&winner.addr)
	c.tr = winner.tr
	c.ctrlMu.Lock()
	c.ctrl = ctrl
	c.ctrlMu.Unlock()
	c.inMu.Lock()
	// 前のストリームで送ったつもりの位置を退避し、サーバから届いた位置を待つ（差が届かなかった入力）
	stream := c.lost.newStream(c.inOffset)
	c.in = in
	c.inConn = winner.conn
	// Dial 直後（この conn に migration が起きる前）に一度だけ読んでキャッシュする。
	c.inDatagramsOK = winner.conn.ConnectionState().SupportsDatagrams.Remote
	c.inOffset = 0 // 新しい入力ストリームなので offset を振り直す（server 側 dedup も接続単位）
	c.inStalled = false
	c.inMu.Unlock()

	go c.pumpOutput(winner.conn)
	go c.readCtrl(ctrl, stream)
	go c.acceptServerStreams(winner.conn)
	return nil
}

// migrate は新しいローカル socket へ能動 migration する（spike B/C の AddPath/Probe/Switch）。
// ストリーム状態は保持されるため、入力/出力/制御ストリームの再構築は不要。
func (c *Client) migrate(reason string) error {
	c.migrateMu.Lock()
	defer c.migrateMu.Unlock()

	// 上限に達したら migration せず reconnect に任せる（maxPathTransports 参照。
	// 新しい conn で張り直せば adoptConn が保持分をまとめて退役させる）。
	if len(c.pathTransports) >= maxPathTransports {
		return fmt.Errorf("migrate: %d paths held on this connection; reconnecting instead", len(c.pathTransports))
	}

	start := time.Now()
	c.setState(StateRecovering, "roaming: "+reason)

	tr, err := newLocalQUICTransport()
	if err != nil {
		return err
	}

	conn := c.conn.Load()
	path, err := conn.AddPath(tr)
	if err != nil {
		_ = closeTransport(tr)
		return err
	}
	// Probe を送った時点で tr にも conn が登録されるため、失敗しても tr は閉じずに
	// 保持する（閉じると、まだ生きている conn まで destroy してしまう）。
	fail := func(err error) error {
		closePath(conn, path)
		c.pathTransports = append(c.pathTransports, tr)
		return err
	}
	// Probe タイムアウトは短め（高遅延でも空振りを長引かせず reconnect へ早く落とす）。
	pctx, cancel := context.WithTimeout(c.ctx, 4*time.Second)
	defer cancel()
	if err := path.Probe(pctx); err != nil {
		return fail(err)
	}
	if err := path.Switch(); err != nil {
		return fail(err)
	}

	// 旧 transport にも conn が登録されたままなので、conn の差し替えまで保持する。
	c.pathTransports = append(c.pathTransports, c.tr)
	c.tr = tr
	d := c.recordRecovery(start)
	c.setState(StateConnected, fmt.Sprintf("roaming: migrated (recovery %dms)", d.Milliseconds()))
	return nil
}

// closePath は migration に失敗した path を閉じる。quic-go v0.60 の Path.Close は
// 接続終了後（connIDManager が Close 済み）に呼ぶと、PATH_CHALLENGE を送った path では
// RetireConnIDForPath の assertNotClosed で panic する（Probe 中に idle timeout 等で
// 接続が死んだ場合に実機で発生）。接続が死んでいれば path を閉じる意味はないので
// 呼ばず、確認と Close の間に接続が死ぬ競合は recover で吸収する。
func closePath(conn *quic.Conn, path *quic.Path) {
	if conn.Context().Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("qtransport: path close after connection closed: %v", r)
		}
	}()
	_ = path.Close()
}

// maxPathTransports は 1 つの conn で migration できる回数の上限（= 保持する
// migration 前 transport の上限）。超えたら migration をやめて reconnect する。
// quic-go v0.60 は path probe ごとにサーバー発行の接続 ID を 1 つ割り当て、検証済み
// path の分は path を閉じても retire しない（removePathImpl）。active_connection_id_limit
// が 4 のため、実測で 3 回目の migration は ID 枯渇で Probe がタイムアウトする。
// 上限で打ち切ることで、その 4 秒を待たずに reconnect へ進む（fd も頭打ちになる）。
const maxPathTransports = 2

// transportRetireGrace は退役した transport（旧 UDP ソケット）を閉じるまでの猶予。
// Switch/reconnect 直後に即閉じすると quic-go 内部のパス処理（旧経路への
// CONNECTION_CLOSE 送出・再送等）と競合しうるため少し待つ。
// テストから短縮できるよう差し替え可能（server.go の stall 水位と同じパターン）。
var transportRetireGrace = newAtomicDuration(30 * time.Second)

// retireTransport は退役した transport を猶予をおいて（または Close 時に即）閉じる。
// Close() まで保持すると、復帰のたびに UDP ソケット fd が溜まる（復帰が頻発すると
// 数日で千個を超える）。
// 対象は conn ごと退役した transport に限る。migration で離れただけの transport は
// conn が登録されたままで、閉じると conn ごと destroy される（pathTransports 参照）。
func (c *Client) retireTransport(tr *quic.Transport) {
	go func() {
		select {
		case <-time.After(transportRetireGrace.Get()):
		case <-c.ctx.Done():
		}
		_ = closeTransport(tr)
	}()
}

// recordRecovery は復帰所要時間と回数を記録し、記録した所要時間を返す
// （呼び出し側が完了ログに埋め込めるように）。
func (c *Client) recordRecovery(start time.Time) time.Duration {
	d := time.Since(start)
	c.lastRecoveryMs.Store(uint64(d.Milliseconds()))
	c.recoveryCount.Add(1)
	return d
}

// pumpOutput は指定 conn のサーバ出力 uni ストリームを受けて Output() へ流す。
// reconnect で conn が差し替わると古い pump は stream エラーで終了し、新しい conn 用の
// pump が起動される。
func (c *Client) pumpOutput(conn *quic.Conn) {
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
		case c.out <- OutputChunk{Offset: offset, Data: data}:
		case <-c.ctx.Done():
			return
		}
	}
}

// 入力 Write の期限。生きた経路の貼り付けは送出に合わせて少しずつ進むので、これだけ
// 進まなければ経路が詰まっているとみなす。詰まった後（inStalled）は打鍵ごとに長く
// 待たないよう短い期限で試す。書けなかった分は捨てる（送信失敗と同じ扱い）。
const (
	inputWriteTimeout        = 5 * time.Second
	inputWriteTimeoutStalled = 100 * time.Millisecond
)

// SendInput sends keyboard input to the session.
func (c *Client) SendInput(data []byte) error {
	// offset とストリーム書き込みの整合を保つため、Write まで inMu 下で行う
	// （実際の呼び出し元は handleStdin の単一 goroutine なので競合はほぼない）。
	c.inMu.Lock()
	in := c.in
	conn := c.inConn
	datagramsOK := c.inDatagramsOK
	offset := c.inOffset
	// 期限を付ける。quic-go の Write は未送出が約 1 パケットを超えると送出されるまでブロック
	// するので、経路が黙って切れている間（検知まで最大 idle timeout）に打鍵や貼り付けが
	// 溜まると stdin の処理ごと止まり、Ctrl-^ . の detach も効かなくなる。
	timeout := inputWriteTimeout
	if c.inStalled {
		timeout = inputWriteTimeoutStalled
	}
	_ = in.SetWriteDeadline(time.Now().Add(timeout))
	n, err := in.Write(data)
	_ = in.SetWriteDeadline(time.Time{})
	// 書けた分だけ進める（期限切れでも途中まではストリームに載っており、サーバの dedup は
	// ストリーム上の位置で数えるので、datagram の offset をそれと揃える）
	c.inOffset += uint64(n)
	c.inStalled = errors.Is(err, os.ErrDeadlineExceeded)
	c.lost.recordSent(data[:n])
	var lostTotal int
	var lostHead []byte
	if err != nil {
		c.lost.addLost(data[n:])
	} else {
		// 再接続なしに経路が戻った（migration 等）なら、送れず捨てた分をここで知らせる
		lostTotal, lostHead = c.lost.take()
	}
	c.inMu.Unlock()
	c.notifyInputLost(lostTotal, lostHead)
	if err != nil {
		// 接続死（スリープ復帰直後など）を打鍵で踏んだら即復帰を起動する
		// （watchdog の次 tick を待たず体感を改善）。多重起動は recovering フラグで弾く。
		// ユーザー操作起点なのでバックオフもリセットして待たせない（ただし間引く）。
		go func() {
			c.resetBackoffByInput()
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

// resolveInputApplied はサーバから届いた前の接続の入力位置（nil = 不明）で、届かなかった
// 入力を確定して知らせる。stream は位置を読んだ接続の入力ストリームの番号。
func (c *Client) resolveInputApplied(applied *uint64, stream uint64) {
	c.inMu.Lock()
	c.lost.resolve(applied, stream)
	total, head := c.lost.take()
	c.inMu.Unlock()
	c.notifyInputLost(total, head)
}

// notifyInputLost は届かなかった入力を OnInputLost で知らせる（total == 0 なら何もしない）。
func (c *Client) notifyInputLost(total int, head []byte) {
	if total > 0 && c.onInputLost != nil {
		c.onInputLost(total, head)
	}
}

// SendResize sends the terminal size.
func (c *Client) SendResize(cols, rows int) error {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()
	return writeFrame(c.ctrl, &ctrlMsg{Type: ctrlResize, Cols: cols, Rows: rows})
}

// Output delivers server output in order. Offset is the session's logical
// offset, the same numbering as OutputMsg.Seq on the Unix-socket path, so
// callers can deduplicate across the two paths.
func (c *Client) Output() <-chan OutputChunk { return c.out }

// State returns the current connection state.
func (c *Client) State() ConnectionState {
	return ConnectionState(c.state.Load())
}

func (c *Client) setState(s ConnectionState, msg string) {
	var old ConnectionState
	for {
		old = ConnectionState(c.state.Load())
		// Close 後は動いていた reconnect / failover が状態を戻さないようにする
		// （終了後に "reconnecting" 等を通知しない）
		if old == s || old == StateClosed {
			return
		}
		if c.state.CompareAndSwap(int32(old), int32(s)) {
			break
		}
	}
	if c.onStateChange != nil {
		c.onStateChange(old, s, msg)
	}
}

// logf はクライアント側で生成した診断メッセージを OnLogMessage 経由で上位
// （クライアントログファイル・stats の通知リスト）へ送る。未設定なら捨てる。
func (c *Client) logf(format string, args ...any) {
	if c.onLogMessage != nil {
		c.onLogMessage(fmt.Sprintf(format, args...))
	}
}

// RemoteAddr returns the server address ("host:port") of the current
// connection, or "" before the first connection.
func (c *Client) RemoteAddr() string {
	if p := c.remoteAddr.Load(); p != nil {
		return *p
	}
	return ""
}

// SetRoamingCandidates replaces the address candidates tried on reconnect
// (obtained via STUN and similar). An empty list is ignored. The slice is
// copied.
func (c *Client) SetRoamingCandidates(addrs []string) {
	if len(addrs) == 0 {
		return
	}
	c.candidatesMu.Lock()
	c.candidates = slices.Clone(addrs)
	c.candidatesMu.Unlock()
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

// readCtrl は control bidi ストリームから server→client の制御フレームを読み、配送する。
// reconnect で control ストリームが張り替わるたびに新しいストリームで起動される。
// stream は同じ接続の入力ストリームの番号（lostInput.newStream の戻り値）。
func (c *Client) readCtrl(ctrl *quic.Stream, stream uint64) {
	for {
		m, err := readFrame(ctrl)
		if err != nil {
			return
		}
		switch m.Type {
		case ctrlServerMeta:
			c.serverFeatures.Store(m.Features)
			if m.Features&FeatureInputApplied == 0 {
				// 旧サーバは位置を送ってこないので待たない（送れず捨てた分だけ知らせる）
				c.resolveInputApplied(nil, stream)
			}
			if c.onServerMeta != nil {
				c.onServerMeta(m.BuildID, m.BuildTime, m.InstanceID)
			}
		case ctrlSessionGone:
			if c.onSessionNotFound != nil {
				exitCode := -1
				if m.ExitCode != nil {
					exitCode = int(*m.ExitCode)
				}
				c.onSessionNotFound(m.Msg, exitCode, m.FinalOffset)
			}
		case ctrlStatus:
			if c.onStatusMessage != nil {
				c.onStatusMessage(m.Msg)
			}
		case ctrlLog:
			if c.onLogMessage != nil {
				c.onLogMessage(m.Msg)
			}
		case ctrlInputApplied:
			c.resolveInputApplied(m.InputApplied, stream)
		case ctrlPong:
			c.lastPongNano.Store(time.Now().UnixNano())
		}
	}
}

// Stats returns connection statistics.
func (c *Client) Stats() Stats {
	st := Stats{
		LastRecoveryMs: float64(c.lastRecoveryMs.Load()),
		RecoveryCount:  c.recoveryCount.Load(),
	}
	if conn := c.conn.Load(); conn != nil {
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

// Close closes the connection.
func (c *Client) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.setState(StateClosed, "closed")
	// c.conn は reconnect / failover（adoptConn）が migrateMu の下で差し替えるので、
	// 同じロックの下で読む（差し替え前の接続にだけ終了を伝えて新しい接続を残さない）。
	c.migrateMu.Lock()
	if conn := c.conn.Load(); conn != nil {
		// 意図した終了であることをサーバへ伝える（-R の listener を休眠させずに手放させる）。
		_ = conn.CloseWithError(connCodeClientExit, "done")
	}
	if c.tr != nil {
		_ = closeTransport(c.tr)
	}
	for _, tr := range c.pathTransports {
		_ = closeTransport(tr)
	}
	c.pathTransports = nil
	// 退役済み transport は retireTransport の goroutine が ctx.Done（上の cancel）を
	// 見て閉じる。
	c.migrateMu.Unlock()
	return nil
}
