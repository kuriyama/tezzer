// quicServer は transport.ServerTransport の QUIC 実装。
//
// プロトコル（v1・最小）:
//   - クライアントが control bidi ストリームを開き Hello{ClientID} を送る → 接続を clientID へ紐付け
//   - 入力: クライアントが uni ストリームを開き生バイトを流す → Input() へ
//   - 出力: サーバが clientID ごとに uni ストリームを開き生バイトを流す（SendOutput）
//   - Resize: control ストリーム上の制御フレーム → Resize() へ
//
// offset ベース再同期（OnResyncNeeded）と migration 追従は後続スライスで実装する。
package qtransport

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/quic-go/quic-go"
)

type serverClient struct {
	id     transport.ClientID
	conn   *quic.Conn
	out    *quic.SendStream
	outMu  sync.Mutex
	ctrl   *quic.Stream // control bidi（server→client の制御フレーム送信に使う）
	ctrlMu sync.Mutex

	// 入力の重複排除（ストリーム + DATAGRAM 二重送信の一度だけ適用）。
	// inMu は dedup 状態と input チャネルへの送信順序の両方を守る
	// （2 つの受信 goroutine が offset 順を崩さないよう、チャネル送信まで保持する）。
	inMu    sync.Mutex
	inDedup inputDeduper

	// 送信健全性（backpressure 観測用、atomic）
	bytesSent    atomic.Uint64
	lastSendNano atomic.Int64
	lastRecvNano atomic.Int64 // クライアントからの最終受信時刻（入力・制御フレーム）
	slowWrites   atomic.Uint64
	maxWriteMs   atomic.Uint64

	// stall 検知（backpressure の即時観測）。slowWrites は完了した Write の統計なので、
	// 死んだ接続への Write（フロー制御ウィンドウを吸い切った後、最大 idle timeout まで
	// ブロック）は完了するまで見えない。writeStartNano でブロック中の Write を
	// stallWatchdog が外から観測できるようにする。
	writeStartNano    atomic.Int64  // 出力 Write の開始時刻（0 = in-flight なし）
	stallEpisodes     atomic.Uint64 // warning 水位超えの累計エピソード数（統計用）
	stallWarned       atomic.Bool   // 現在のエピソードで計上済み（Write 完了でリセット）
	lastStallWarnNano atomic.Int64  // 最後に他クライアントへ通知した時刻（レートリミット）

	// 転送統計（forward.go。fwdActive は maxForwardsPerClient の制限にも使う）
	fwdActive          atomic.Int32
	fwdOpened          atomic.Uint64 // 累計転送接続数（dial 成功分）
	fwdBytesToTarget   atomic.Uint64 // client → target 方向の累計バイト
	fwdBytesFromTarget atomic.Uint64 // target → client 方向の累計バイト

	// agent forwarding（-A）。Hello の AgentForward で立つ（agent.go）。
	agentForward bool
}

type quicServer struct {
	ln     *quic.Listener
	tr     *quic.Transport // 自前 UDP ソケット上の transport（Close で明示的に閉じる）
	uconn  net.PacketConn  // 待ち受け UDP ソケット（無停止再起動時に fd を継承する）
	ctx    context.Context
	cancel context.CancelFunc

	input  chan transport.Input
	resize chan transport.Resize

	mu      sync.Mutex
	clients map[transport.ClientID]*serverClient

	onConnect    func(transport.ClientID)
	onDisconnect func(transport.ClientID)
	onResync     func(transport.ClientID, uint64) ([]transport.OutputChunk, error)

	// クライアント接続時に通知するサーバ情報（SetServerMeta で設定）
	metaBuildID    string
	metaBuildTime  string
	metaInstanceID []byte

	// TCP 転送の禁止フラグ（SetTCPForwarding。零値 = 許可）
	fwdDisabled atomic.Bool
	// agent forwarding の禁止フラグ（SetAgentForwarding。零値 = 許可。agent.go）
	agentFwdDisabled atomic.Bool
	// セッションごとの現在の agent forwarding provider（agent.go。mu で保護）
	agentProviders map[string]transport.ClientID
}

// NewServer は listenAddr で待ち受ける QUIC サーバトランスポートを作る。
// UDP ソケットは自前で開く（無停止再起動時に fd を取り出して継承するため）。
func NewServer(k []byte, listenAddr string) (transport.ServerTransport, error) {
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, err
	}
	uconn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	st, err := NewServerFromPacketConn(k, uconn)
	if err != nil {
		_ = uconn.Close()
		return nil, err
	}
	return st, nil
}

// NewServerFromPacketConn は既存の UDP ソケット上に QUIC サーバトランスポートを作る
// （無停止再起動での fd 継承用。ソケットの所有権は quicServer に移り、Close で閉じられる）。
func NewServerFromPacketConn(k []byte, uconn net.PacketConn) (transport.ServerTransport, error) {
	tlsConf, err := ServerTLS(k)
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: uconn}
	ln, err := tr.Listen(tlsConf, quicConfig())
	if err != nil {
		return nil, err
	}
	return &quicServer{
		ln:             ln,
		tr:             tr,
		uconn:          uconn,
		input:          make(chan transport.Input, 256),
		resize:         make(chan transport.Resize, 16),
		clients:        make(map[transport.ClientID]*serverClient),
		agentProviders: make(map[string]transport.ClientID),
	}, nil
}

// DisconnectAllClients は全クライアントの QUIC 接続を CONNECTION_CLOSE で切断する。
// 無停止再起動の直前に呼ぶことで、クライアントは idle timeout（60秒）を待たずに
// 即座に接続死を検知し、reconnect 機構で新プロセスへ再接続できる。
func (s *quicServer) DisconnectAllClients(reason string) {
	s.mu.Lock()
	conns := make(map[*quic.Conn]bool)
	for _, sc := range s.clients {
		if sc.conn != nil {
			conns[sc.conn] = true
		}
	}
	s.mu.Unlock()
	for conn := range conns {
		_ = conn.CloseWithError(0, reason)
	}
}

// DupUDPSocketFd は待ち受け UDP ソケットを複製した fd（CLOEXEC なし）を返す。
// 無停止再起動（self re-exec）で新プロセスへ継承するために使う。
func (s *quicServer) DupUDPSocketFd() (int, error) {
	sc, ok := s.uconn.(syscall.Conn)
	if !ok {
		return -1, errors.New("udp socket does not support SyscallConn")
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	var dupErr error
	if err := raw.Control(func(cfd uintptr) {
		// dup(2) は FD_CLOEXEC を複製しない = 新 fd は exec を跨いで継承される
		fd, dupErr = syscall.Dup(int(cfd))
	}); err != nil {
		return -1, err
	}
	return fd, dupErr
}

// Addr は待ち受けアドレスを返す（インターフェース外。bootstrap で client へ伝える用）。
func (s *quicServer) Addr() net.Addr { return s.ln.Addr() }

func (s *quicServer) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)
	go s.acceptLoop()
	go s.stallWatchdog()
	return nil
}

func (s *quicServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept(s.ctx)
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *quicServer) handleConn(conn *quic.Conn) {
	// 最初の bidi ストリーム = control。Hello を読んで clientID を確定する。
	ctrl, err := conn.AcceptStream(s.ctx)
	if err != nil {
		return
	}
	m, err := readFrame(ctrl)
	if err != nil || m.Type != ctrlHello {
		_ = conn.CloseWithError(1, "expected hello")
		return
	}
	cid := transport.ClientID{Session: m.SessionID, Num: m.ClientID}

	// このクライアントへの出力 uni ストリームを開く。
	out, err := conn.OpenUniStreamSync(s.ctx)
	if err != nil {
		return
	}
	sc := &serverClient{id: cid, conn: conn, out: out, ctrl: ctrl, agentForward: m.AgentForward}

	s.mu.Lock()
	s.clients[cid] = sc
	if m.AgentForward {
		// 「最後の Hello が勝つ」: migration/reconnect でも同じ規約で上書きする。
		s.agentProviders[cid.Session] = cid
	}
	onConnect := s.onConnect
	onResync := s.onResync
	buildID, buildTime, instanceID := s.metaBuildID, s.metaBuildTime, s.metaInstanceID
	s.mu.Unlock()
	if onConnect != nil {
		onConnect(cid)
	}

	// サーバ情報を control ストリームで通知（UDS 非依存。スリープで UDS が切れても届く）。
	// Features（転送対応等）を運ぶため常に送る。
	_ = s.writeCtrl(sc, &ctrlMsg{Type: ctrlServerMeta, BuildID: buildID, BuildTime: buildTime, InstanceID: instanceID, Features: s.features()})

	// 再同期: クライアントが最後に受けた offset の次から、セッション層（OutputRingBuffer）に
	// 残っている分を先行送信してからライブ出力に入る。重複は client 側が offset で弾く。
	// セッション層はバッチ（raw バイト上限つき）で返すため、空が返るまで繰り返し引く
	// （cold 層全量を一括で解凍・保持しない。メモリスパイク防止）。
	if onResync != nil {
		next := m.LastOffset + 1
	resync:
		for {
			chunks, err := onResync(cid, next)
			if err != nil || len(chunks) == 0 {
				break
			}
			for _, ch := range chunks {
				// writeOutput 経由にすることで、再同期の Write がブロックした場合も
				// stallWatchdog から観測できる（outMu 越しに SendOutput = PTY reader
				// まで詰まらせるのは同じため）。
				if _, werr := sc.writeOutput(ch.Offset, ch.Data); werr != nil {
					break resync
				}
				next = ch.Offset + 1
			}
		}
	}

	go s.readControl(sc, ctrl)
	go s.readInput(sc)
	go s.readInputDatagrams(sc)
	go s.acceptForwardStreams(sc)

	<-conn.Context().Done()

	// 同一 clientID が別接続で再接続済みの場合、この古い接続の終了で新しい
	// serverClient を消さないようにする（長スリープ後の reconnect 対策）。
	s.mu.Lock()
	stillMine := s.clients[cid] == sc
	if stillMine {
		delete(s.clients, cid)
		if sc.agentForward && s.agentProviders[cid.Session] == cid {
			// 他に -A 付きで attach 中のクライアントがいれば委譲、いなければ provider なし。
			delete(s.agentProviders, cid.Session)
			for otherID, otherSC := range s.clients {
				if otherID.Session == cid.Session && otherSC.agentForward {
					s.agentProviders[cid.Session] = otherID
					break
				}
			}
		}
	}
	onDisconnect := s.onDisconnect
	s.mu.Unlock()
	if stillMine && onDisconnect != nil {
		onDisconnect(cid)
	}
}

// readControl は control ストリームの後続フレーム（Resize 等）を読む。
func (s *quicServer) readControl(sc *serverClient, ctrl *quic.Stream) {
	for {
		m, err := readFrame(ctrl)
		if err != nil {
			return
		}
		sc.lastRecvNano.Store(time.Now().UnixNano())
		if m.Type == ctrlResize {
			select {
			case s.resize <- transport.Resize{Client: sc.id, Cols: m.Cols, Rows: m.Rows}:
			case <-s.ctx.Done():
				return
			}
		}
	}
}

// readInput はクライアントの入力 uni ストリームを受けて Input() へ流す。
// DATAGRAM で先に適用済みのバイトは dedup が落とす。
func (s *quicServer) readInput(sc *serverClient) {
	rs, err := sc.conn.AcceptUniStream(s.ctx)
	if err != nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := rs.Read(buf)
		if n > 0 {
			sc.lastRecvNano.Store(time.Now().UnixNano())
			sc.inMu.Lock()
			out := sc.inDedup.fromStream(buf[:n])
			if len(out) > 0 {
				// buf は再利用するのでコピーして渡す。
				data := append([]byte(nil), out...)
				select {
				case s.input <- transport.Input{Client: sc.id, Data: data}:
				case <-s.ctx.Done():
					sc.inMu.Unlock()
					return
				}
			}
			sc.inMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// readInputDatagrams は投機的二重送信された入力 DATAGRAM を受けて Input() へ流す。
// ストリームより先に届いた場合だけ適用され、遅れて届いた重複は dedup が落とす。
func (s *quicServer) readInputDatagrams(sc *serverClient) {
	if !sc.conn.ConnectionState().SupportsDatagrams.Local {
		return
	}
	for {
		b, err := sc.conn.ReceiveDatagram(s.ctx)
		if err != nil {
			return
		}
		offset, data, ok := decodeInputDatagram(b)
		if !ok {
			continue
		}
		sc.lastRecvNano.Store(time.Now().UnixNano())
		sc.inMu.Lock()
		out := sc.inDedup.fromDatagram(offset, data)
		if len(out) > 0 {
			// b は ReceiveDatagram ごとに新規割り当てなのでコピー不要。
			select {
			case s.input <- transport.Input{Client: sc.id, Data: out}:
			case <-s.ctx.Done():
				sc.inMu.Unlock()
				return
			}
		}
		sc.inMu.Unlock()
	}
}

// slowWriteThreshold を超える出力 Write は backpressure（遅いクライアントが
// フロー制御で詰まっている）の兆候として slowWrites に計上する。
const slowWriteThreshold = 100 * time.Millisecond

// stall 検知の水位（テストから短縮できるよう var）。
// warning 水位を超えてブロックしている Write を見つけたら、同セッションの
// 他クライアントへステータス通知する（詰まっている本人には届かない・見えないため、
// 「固まった画面を見ている側」に犯人を知らせる）。
// critical 水位を超えたら、その クライアントの QUIC 接続を切断して PTY reader を
// 解放する。切られたクライアントは既存の reconnect + offset 再同期で復旧するため、
// リングバッファ保持分のデータは失われない（スリープ中のノート PC が典型で、
// 復帰時にどのみち reconnect する。MaxIdleTimeout=60s より十分手前で切る）。
var (
	stallWarnThreshold     = 2 * time.Second
	stallWarnRepeat        = 30 * time.Second // stall 継続中の再通知間隔
	stallCriticalThreshold = 30 * time.Second // 超過で当該クライアントを切断
	stallCheckInterval     = 1 * time.Second
)

// connCodeStallDisconnect は critical stall による切断の CONNECTION_CLOSE コード
// （デバッグ用途。プロトコル上の意味は持たせない）。
const connCodeStallDisconnect quic.ApplicationErrorCode = 2

// writeOutput は出力フレームを 1 つ書き、所要時間を返す。書き込み開始時刻を
// writeStartNano に記録し、ブロック中の Write を stallWatchdog が外から観測できる
// ようにする。完了時、このエピソードで stall 計上済みならリセットして復帰をログする。
func (sc *serverClient) writeOutput(offset uint64, data []byte) (time.Duration, error) {
	sc.outMu.Lock()
	t0 := time.Now()
	sc.writeStartNano.Store(t0.UnixNano())
	err := writeOutputFrame(sc.out, offset, data)
	dur := time.Since(t0)
	sc.writeStartNano.Store(0)
	sc.outMu.Unlock()
	if sc.stallWarned.Swap(false) {
		sc.lastStallWarnNano.Store(0)
		log.Printf("qtransport: client %d (session %s) output resumed after %v stall (err=%v)",
			sc.id.Num, sc.id.Session, dur.Truncate(time.Millisecond), err)
	}
	return dur, err
}

func (s *quicServer) SendOutput(offset uint64, data []byte, clients []transport.ClientID) error {
	for _, cid := range clients {
		s.mu.Lock()
		sc := s.clients[cid]
		s.mu.Unlock()
		if sc == nil {
			continue
		}
		dur, err := sc.writeOutput(offset, data)
		if err != nil {
			// 1 クライアントの失敗で他を止めない（切断は handleConn 側で検知）。
			continue
		}
		// 送信健全性を記録（backpressure 観測）。
		sc.bytesSent.Add(uint64(len(data)))
		sc.lastSendNano.Store(time.Now().UnixNano())
		if ms := uint64(dur.Milliseconds()); ms > sc.maxWriteMs.Load() {
			sc.maxWriteMs.Store(ms)
		}
		if dur >= slowWriteThreshold {
			sc.slowWrites.Add(1)
		}
	}
	return nil
}

// stallWatchdog は warning 水位を超えてブロックしている出力 Write を定期的に探し、
// 同セッションの他クライアントへステータス通知する。SendOutput は PTY reader から
// 同期的に呼ばれるため、1 クライアントの stall はセッション全体の出力停止として
// 他クライアントのユーザーに見えている（固まった画面の理由をその場で知らせる）。
// critical 水位を超えたら当該クライアントを切断し、セッション全体を解放する。
func (s *quicServer) stallWatchdog() {
	// 水位はここで一度だけ読む（テストが Start 前に差し替え、終了後に復元するため）。
	warnThreshold, warnRepeat := stallWarnThreshold, stallWarnRepeat
	criticalThreshold := stallCriticalThreshold
	ticker := time.NewTicker(stallCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		s.mu.Lock()
		snapshot := make([]*serverClient, 0, len(s.clients))
		for _, sc := range s.clients {
			snapshot = append(snapshot, sc)
		}
		s.mu.Unlock()
		for _, sc := range snapshot {
			start := sc.writeStartNano.Load()
			if start == 0 {
				continue
			}
			stalled := now.Sub(time.Unix(0, start))
			if stalled < warnThreshold {
				continue
			}
			// critical 水位: 詰まったクライアントを切断して PTY reader を解放する。
			// CONNECTION_CLOSE でブロック中の writeOutput は即エラー復帰し、
			// クライアント側は既存の reconnect + offset 再同期で復旧する
			// （リングバッファ保持分のデータ損失なし）。接続が実際に落ちて
			// handleConn が後始末するまで数 tick 見え続けるが、CloseWithError の
			// 再呼び出しは no-op なので問題ない。
			if stalled >= criticalThreshold {
				log.Printf("qtransport: client %d (session %s) output stalled %v (critical); disconnecting",
					sc.id.Num, sc.id.Session, stalled.Truncate(time.Millisecond))
				if sc.conn != nil {
					_ = sc.conn.CloseWithError(connCodeStallDisconnect, "output stalled: disconnecting slow client")
				}
				continue
			}
			if sc.stallWarned.CompareAndSwap(false, true) {
				sc.stallEpisodes.Add(1)
			} else if now.UnixNano()-sc.lastStallWarnNano.Load() < int64(warnRepeat) {
				continue
			}
			sc.lastStallWarnNano.Store(now.UnixNano())
			addr := "?"
			if sc.conn != nil {
				if a := sc.conn.RemoteAddr(); a != nil {
					addr = a.String()
				}
			}
			msg := fmt.Sprintf("output stalled %ds by client %d (%s) not reading",
				int(stalled/time.Second), sc.id.Num, addr)
			log.Printf("qtransport: session %s: %s", sc.id.Session, msg)
			for _, other := range snapshot {
				if other == sc || other.id.Session != sc.id.Session {
					continue
				}
				if other.writeStartNano.Load() != 0 {
					continue // 自分も詰まっているクライアントには送らない
				}
				// control ストリームが万一詰まっていても watchdog を止めないよう
				// goroutine で送る（レートリミット済みなので数は増えない）。
				go func(o *serverClient) {
					_ = s.writeCtrl(o, &ctrlMsg{Type: ctrlStatus, Msg: msg})
				}(other)
			}
		}
	}
}

func (s *quicServer) ClientSendStats() []transport.ClientSendStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := make([]transport.ClientSendStat, 0, len(s.clients))
	for id, sc := range s.clients {
		var lastUnix int64
		if n := sc.lastSendNano.Load(); n != 0 {
			lastUnix = n / int64(time.Second)
		}
		if n := sc.lastRecvNano.Load(); n != 0 {
			if recv := n / int64(time.Second); recv > lastUnix {
				lastUnix = recv
			}
		}
		var quicRemoteAddr string
		if sc.conn != nil {
			if addr := sc.conn.RemoteAddr(); addr != nil {
				quicRemoteAddr = addr.String()
			}
		}
		// 進行中の stall（warning 水位超えのみ。通常の in-flight Write は µs 単位で
		// ノイズになるため報告しない）
		var curStallMs uint64
		if start := sc.writeStartNano.Load(); start != 0 {
			if d := time.Since(time.Unix(0, start)); d >= stallWarnThreshold {
				curStallMs = uint64(d.Milliseconds())
			}
		}
		stats = append(stats, transport.ClientSendStat{
			Client:                 id,
			BytesSent:              sc.bytesSent.Load(),
			LastSendUnix:           lastUnix,
			SlowWrites:             sc.slowWrites.Load(),
			MaxWriteMs:             sc.maxWriteMs.Load(),
			StallEpisodes:          sc.stallEpisodes.Load(),
			CurrentStallMs:         curStallMs,
			QUICRemoteAddr:         quicRemoteAddr,
			ForwardsActive:         sc.fwdActive.Load(),
			ForwardsOpened:         sc.fwdOpened.Load(),
			ForwardBytesToTarget:   sc.fwdBytesToTarget.Load(),
			ForwardBytesFromTarget: sc.fwdBytesFromTarget.Load(),
		})
	}
	return stats
}

// writeCtrl はクライアントの control bidi ストリームへ制御フレームを書く（server→client）。
func (s *quicServer) writeCtrl(sc *serverClient, m *ctrlMsg) error {
	if sc.ctrl == nil {
		return nil
	}
	sc.ctrlMu.Lock()
	defer sc.ctrlMu.Unlock()
	return writeFrame(sc.ctrl, m)
}

func (s *quicServer) SetServerMeta(buildID, buildTime string, instanceID []byte) {
	s.mu.Lock()
	s.metaBuildID = buildID
	s.metaBuildTime = buildTime
	s.metaInstanceID = instanceID
	s.mu.Unlock()
}

func (s *quicServer) SendSessionGone(client transport.ClientID, reason string, exitCode int) error {
	s.mu.Lock()
	sc := s.clients[client]
	s.mu.Unlock()
	if sc == nil {
		return nil
	}
	m := &ctrlMsg{Type: ctrlSessionGone, Msg: reason}
	if exitCode >= 0 {
		ec := int32(exitCode)
		m.ExitCode = &ec
	}
	return s.writeCtrl(sc, m)
}

func (s *quicServer) SendStatus(client transport.ClientID, msg string) error {
	s.mu.Lock()
	sc := s.clients[client]
	s.mu.Unlock()
	if sc == nil {
		return nil
	}
	return s.writeCtrl(sc, &ctrlMsg{Type: ctrlStatus, Msg: msg})
}

func (s *quicServer) Input() <-chan transport.Input   { return s.input }
func (s *quicServer) Resize() <-chan transport.Resize { return s.resize }

func (s *quicServer) OnClientConnect(fn func(transport.ClientID)) {
	s.mu.Lock()
	s.onConnect = fn
	s.mu.Unlock()
}
func (s *quicServer) OnClientDisconnect(fn func(transport.ClientID)) {
	s.mu.Lock()
	s.onDisconnect = fn
	s.mu.Unlock()
}
func (s *quicServer) OnResyncNeeded(fn func(transport.ClientID, uint64) ([]transport.OutputChunk, error)) {
	s.mu.Lock()
	s.onResync = fn
	s.mu.Unlock()
}

func (s *quicServer) ActiveClients() []transport.ClientID {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]transport.ClientID, 0, len(s.clients))
	for id := range s.clients {
		ids = append(ids, id)
	}
	return ids
}

func (s *quicServer) Stats() transport.Stats { return transport.Stats{} }

func (s *quicServer) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	err := s.ln.Close()
	// tr は struct literal で構築しており quic-go が Conn を閉じないため明示的に閉じる
	// （closeTransport と同じ理由。fd リーク防止）。
	if s.tr != nil {
		if terr := s.tr.Close(); terr != nil && err == nil {
			err = terr
		}
	}
	if s.uconn != nil {
		if cerr := s.uconn.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

var (
	_ transport.ServerTransport  = (*quicServer)(nil)
	_ transport.ForwardingPolicy = (*quicServer)(nil)
	_ transport.SocketHandover   = (*quicServer)(nil)
	_ transport.Addresser        = (*quicServer)(nil)
)
