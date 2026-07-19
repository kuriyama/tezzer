package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/qtransport"
	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/kuriyama/tezzer/internal/version"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	// hot 層: 直近出力を生のまま保持する上限。ローミング・短時間切断の
	// 即時再同期と新規 attach の初期再生を担当する性能パスで、追記時の
	// アロケーションがチャンクコピー 1 回で済む程度に小さく保つ。
	maxHotOutputBytes  = 4 * 1024 * 1024
	maxHotOutputChunks = 8192
	// cold 層: hot からあふれた分を flate 圧縮して保持する上限（圧縮後バイト数）。
	// 端末出力は圧縮がよく効くため raw 換算では数百 MB 相当になる。
	maxColdOutputBytes = 32 * 1024 * 1024
	// cold セグメント 1 つに連結する raw バイト数の目安。これだけ貯まったら圧縮する。
	coldSegmentRawTarget = 1 * 1024 * 1024
	// チャンクの最大保持時間（hot/cold 共通）。「1〜3 日閉じたモバイルノートの
	// 復帰を違和感なく」を保持期間の根拠にする。
	maxOutputBufferAge = 72 * time.Hour
	// 出力が止まったとき、圧縮待ち（coldPending）を圧縮に回すまでの猶予
	//（Manager の cleanup ticker で判定）。
	coldPendingFlushAge = 2 * time.Minute
	// 再同期（outputFromOffset）1 バッチの raw バイト上限の目安。transport は空が
	// 返るまで繰り返し引く契約なので全量は最終的に届くが、cold 層（raw 換算で
	// 数百 MB になりうる）を一括で解凍・保持するメモリスパイクを避ける。
	// cold セグメント（raw 約 1MB）単位で判定するため、実バッチは上限を多少超えうる。
	maxResyncBatchRawBytes = 4 * 1024 * 1024
)

// デバッグ出力のパッケージレベル制御（atomic操作用、0=OFF, 1=ON）
var debugEnabled int32

// SetDebugEnabled enables or disables debug output.
func SetDebugEnabled(enabled bool) {
	var val int32
	if enabled {
		val = 1
	}
	atomic.StoreInt32(&debugEnabled, val)
}

// IsDebugEnabled reports whether debug output is enabled.
func IsDebugEnabled() bool {
	return atomic.LoadInt32(&debugEnabled) == 1
}

// Session represents a PTY session
type Session struct {
	ID        string
	Name      string // session name (-name; empty = unnamed; set at creation, immutable)
	Cmd       string
	Args      []string
	Rows      int
	Cols      int
	CreatedAt time.Time

	mu        sync.RWMutex
	ptyMaster *os.File
	cmd       *exec.Cmd
	// proc はセッションの子プロセス。通常は StartProcess で cmd.Process から設定される。
	// 無停止再起動（self re-exec）で復元されたセッションは cmd を持たず proc のみ持つ
	// （exec はプロセス置き換えなので親子関係が維持され、proc.Wait() で回収できる）。
	proc *os.Process
	seq  uint64

	// 出力チャンクのリングバッファ（再接続時の復帰用）
	// hot 層: 直近出力を生のまま保持（即時再同期の性能パス）
	outputChunks      []OutputChunk
	outputBufferBytes int // outputChunksの合計バイト数
	// cold 層: hot からあふれた分の flate 圧縮セグメント（数日スリープ用の容量パス）
	coldPending      []OutputChunk // 圧縮待ちの生チャンク（coldSegmentRawTarget まで貯める）
	coldPendingBytes int
	coldFlushing     bool // flushColdPending がロック外で圧縮中（先頭バッチが in-flight）
	coldSegments     []coldSegment
	coldBytes        int // coldSegments の圧縮後合計バイト数
	coldRawBytes     int // coldSegments の圧縮前合計バイト数（統計用）

	// Client fanout
	clients      map[string]*Client
	clientIDSeed int

	// トランスポート（QUIC）。per-session で 1 つ、または manager の共有 transport を参照。
	st                  transport.ServerTransport
	stCancel            context.CancelFunc
	usesSharedTransport bool // true なら st は manager 所有の共有 transport（Close/ループは manager 管理）
	quicEnabled         bool
	quicPort            int
	quicKey             []byte
	ipv4Only            bool     // IPv4のみ使用する場合true
	quicClientIDsOut    []uint16 // QUIC 出力ファンアウト対象の clientID リスト（RegisterQUICClient で登録）

	// Manager参照（共有 transport モード時のClientIDルーティング用）
	manager *Manager

	// SSH agent forwarding（-A）。作成時にのみ確定し、以後不変（agent.go）。
	agentListener *net.UnixListener
	agentSockPath string
	agentActive   atomic.Int32 // 同時中継数（maxAgentStreams の制限に使う）

	// QUIC 確立待ち（新規セッション用）
	// UDS への PTY 出力配信を抑制し、QUIC 経由でのみ DA クエリを届けることで
	// UDS+QUIC の二重経路による DA 応答漏洩を防ぐ。
	// UDPClientInfoMsg 受信（clientID 登録）と QUIC onConnect の両方が揃ってから解除する。
	quicReady              atomic.Bool   // 両条件が揃ったら true
	quicReadyCh            chan struct{} // 同上で close される（StartProcess トリガー）
	quicUDSInfoReceived    bool          // UDPClientInfoMsg 受信済み（mu 保護）
	quicTransportConnected bool          // QUIC onConnect 発火済み（mu 保護）

	// Shutdown
	done        chan struct{}
	closeOnce   sync.Once
	ptyClosed   bool      // PTYが終了したかどうか
	ptyClosedAt time.Time // PTY終了時刻
	exitCode    int       // プロセスの終了コード（-1 = 未終了 or 不明。シグナル死は 128+signal）

	// デタッチ追跡
	lastDetachedAt time.Time // 最後にクライアントが0になった時刻

	// 活動追跡（freshness）: セッション単位の最終 PTY 出力/入力時刻。
	// クライアントが誰も attach していなくても更新される点が ClientInfo.LastSeen
	// との違い。「セッション内のプロセスが沈黙して入力待ちになっていないか」を
	// 外部スクリプトが判定する材料として -list/-info に出す。
	lastOutputAt time.Time // 最後に PTY が出力した時刻
	lastInputAt  time.Time // 最後に PTY へ入力を書いた時刻

	// Debug
	debug bool // デバッグログ出力フラグ
}

// OutputChunk is one piece of output (bytes read from the PTY).
type OutputChunk struct {
	Seq       uint64
	Data      []byte
	Timestamp time.Time // creation time
}

// Client represents a connected client
type Client struct {
	ID           string
	Session      *Session
	OutCh        chan []byte
	Done         chan struct{} // used by the writer goroutine to detect detach
	QUICClientID uint16        // client identifier for the QUIC path (0 = QUIC unused)
	Protocol     string        // "UDS", "TCP", "UDP"
	RemoteAddr   string        // remote address (TCP/UDP)
}

// lookupLoginShell は /etc/passwd からユーザーのログインシェルを返す。
// 失敗時は空文字を返す。
func lookupLoginShell(username string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) >= 7 && fields[0] == username {
			return fields[6]
		}
	}
	return ""
}

// systemd サービス固有の変数（セッションに渡すべきでない）
var systemdEnvKeys = map[string]bool{
	"INVOCATION_ID":         true,
	"JOURNAL_STREAM":        true,
	"SYSTEMD_EXEC_PID":      true,
	"MEMORY_PRESSURE_WATCH": true,
	"MEMORY_PRESSURE_WRITE": true,
	"MANAGERPID":            true,
}

// NewSession creates a new session. Unless the manager runs a shared
// transport, it starts a per-session QUIC transport.
func NewSession(id, cmd string, args []string, env map[string]string, cwd string, rows, cols int, ipv4Only bool, fixedUDPPort int, agentForward bool, mgr *Manager) (*Session, error) {
	// Create command
	command := exec.Command(cmd, args...)
	if cwd != "" {
		command.Dir = cwd
	}
	// 環境変数を設定: 親プロセスの環境変数を継承し、envで上書きする
	// まずenvのキーをmapに集める（上書き対象の検出用）
	envOverrides := make(map[string]string)
	for k, v := range env {
		envOverrides[k] = v
	}
	// 親プロセスの環境変数を追加（ただしenvで上書きされるものと systemd 固有変数は除く）
	for _, e := range os.Environ() {
		// "KEY=VALUE" 形式から KEY を抽出
		idx := 0
		for idx < len(e) && e[idx] != '=' {
			idx++
		}
		if idx < len(e) {
			key := e[:idx]
			if _, overridden := envOverrides[key]; overridden {
				continue
			}
			if systemdEnvKeys[key] {
				continue
			}
			command.Env = append(command.Env, e)
		}
	}
	// envの値を追加（上書き）
	for k, v := range env {
		command.Env = append(command.Env, fmt.Sprintf("%s=%s", k, v))
	}
	// SHELL を /etc/passwd のログインシェルで上書き（クライアントが明示指定しない場合）
	if _, ok := envOverrides["SHELL"]; !ok {
		if u, err := user.Current(); err == nil {
			if shell := lookupLoginShell(u.Username); shell != "" {
				command.Env = append(command.Env, "SHELL="+shell)
			}
		}
	}

	// tezzerセッション内からの再帰接続を検出するため、セッションIDを環境変数に設定
	command.Env = append(command.Env, fmt.Sprintf("TEZZER_SESSION=%s", id))

	// agent forwarding（-A）: 要求されており、かつサーバが許可していれば per-session UDS を
	// bind し、SSH_AUTH_SOCK をその場で上書きする（クライアントの明示指定より常に優先。
	// -A の意味そのものが「このソケットへ中継する」なので、値の食い違いは許容しない）。
	// bind 失敗は forwarding なしで継続する（PTY 自体は起動させたい。ssh の -A 失敗時と同様）。
	var agentListener *net.UnixListener
	var agentSockPath string
	if agentForward && (mgr == nil || mgr.AgentForwardingEnabled()) {
		ln, path, err := newAgentListener(id)
		if err != nil {
			log.Printf("session %s: agent forwarding requested but socket bind failed: %v", id, err)
		} else {
			agentListener = ln
			agentSockPath = path
			command.Env = append(command.Env, "SSH_AUTH_SOCK="+path)
		}
	}

	s := &Session{
		ID:            id,
		Cmd:           cmd,
		Args:          args,
		Rows:          rows,
		Cols:          cols,
		CreatedAt:     time.Now(),
		cmd:           command,
		seq:           0,
		outputChunks:  make([]OutputChunk, 0, 256),
		clients:       make(map[string]*Client),
		done:          make(chan struct{}),
		ipv4Only:      ipv4Only,
		debug:         IsDebugEnabled(),
		manager:       mgr,
		quicReadyCh:   make(chan struct{}),
		exitCode:      -1,
		agentListener: agentListener,
		agentSockPath: agentSockPath,
	}
	// デフォルトは「QUIC 準備完了」状態（= UDS 出力通過）。
	// tezzerd の新規セッション作成フローのみ BeginQuicPendingMode() で抑制モードに入る。
	s.quicReady.Store(true)

	// per-session の QUIC トランスポートを起動。
	if err := s.startQUICTransport(fixedUDPPort); err != nil {
		log.Printf("session %s: failed to start QUIC transport (continuing without QUIC): %v", s.ID, err)
		s.quicEnabled = false
	}

	if s.agentListener != nil {
		go s.acceptAgentConns()
	}

	return s, nil
}

// StartProcess starts the PTY process. Calling it after NewSession, right
// after the client has attached (AttachClient), guarantees that the startup
// sequences of programs like tmux (including DA2 queries) reach the client.
func (s *Session) StartProcess() error {
	ptyMaster, err := pty.Start(s.cmd)
	if err != nil {
		return fmt.Errorf("failed to start pty: %w", err)
	}
	if err := pty.Setsize(ptyMaster, &pty.Winsize{Rows: uint16(s.Rows), Cols: uint16(s.Cols)}); err != nil {
		ptyMaster.Close()
		return fmt.Errorf("failed to resize pty: %w", err)
	}
	s.mu.Lock()
	s.ptyMaster = ptyMaster
	s.proc = s.cmd.Process
	s.mu.Unlock()
	go s.ptyReader()
	return nil
}

// startQUICTransport は per-session の QUIC トランスポートを起動する。
// （関数名は bootstrap 互換のため据え置き。中身は qtransport。）
func (s *Session) startQUICTransport(fixedPort int) error {
	// 共有モード（固定ポート運用）: manager 所有の共有 transport を参照する。
	// 入力/リサイズのルーティングと OnResyncNeeded は manager 側が担当するため、
	// ここでは I/O ループを起動しない。
	if s.manager != nil && s.manager.IsSharedTransportEnabled() {
		s.st = s.manager.sharedTransport
		s.usesSharedTransport = true
		s.quicEnabled = true
		s.quicPort = s.manager.GetSharedPort()
		s.quicKey = s.manager.GetSharedKey()
		log.Printf("session %s: using shared QUIC transport on port %d", s.ID, s.quicPort)
		return nil
	}

	// 共有鍵を生成（mTLS の peer 認証に使用）。
	sharedKey := make([]byte, 32)
	if _, err := rand.Read(sharedKey); err != nil {
		return fmt.Errorf("failed to generate shared key: %w", err)
	}

	// ListenAddrを決定（固定ポート指定がある場合はそれを使用、失敗時は :0 フォールバック）
	listenAddr := ":0"
	if fixedPort > 0 {
		listenAddr = fmt.Sprintf(":%d", fixedPort)
	}

	st, err := qtransport.NewServer(sharedKey, listenAddr)
	if err != nil && fixedPort > 0 {
		log.Printf("session %s: fixed port %d unavailable, falling back to auto-assign: %v", s.ID, fixedPort, err)
		st, err = qtransport.NewServer(sharedKey, ":0")
	}
	if err != nil {
		return fmt.Errorf("failed to create QUIC transport: %w", err)
	}

	a, ok := st.(transport.Addresser)
	if !ok {
		_ = st.Close()
		return fmt.Errorf("transport does not expose listen address")
	}
	addr, ok := a.Addr().(*net.UDPAddr)
	if !ok {
		_ = st.Close()
		return fmt.Errorf("failed to get QUIC listen address")
	}

	return s.adoptPerSessionTransport(st, addr.Port, sharedKey)
}

// adoptPerSessionTransport は per-session QUIC トランスポートの配線・起動を行う
// （startQUICTransport と無停止再起動の復元経路で共通）。失敗時は st を閉じる。
func (s *Session) adoptPerSessionTransport(st transport.ServerTransport, port int, key []byte) error {
	// 再同期: クライアント接続時に OutputRingBuffer から欠損分を埋める（Start 前に配線）。
	st.OnResyncNeeded(s.outputFromOffset)
	// サーバ情報をクライアントへ通知する（UDS 非依存・QUIC 経路）。
	var instanceID []byte
	if s.manager != nil {
		id := s.manager.GetServerInstanceID()
		instanceID = id[:]
	}
	st.SetServerMeta(version.GetVersion(), version.GetBuildTime(), instanceID)
	if s.manager != nil {
		s.manager.applyTransportPolicy(st)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := st.Start(ctx); err != nil {
		cancel()
		_ = st.Close()
		return fmt.Errorf("failed to start QUIC transport: %w", err)
	}

	s.st = st
	s.stCancel = cancel
	s.quicEnabled = true
	s.quicPort = port
	s.quicKey = key

	log.Printf("session %s: QUIC transport enabled on port %d", s.ID, s.quicPort)

	// QUIC クライアント接続時に Gate 2 を通知（BeginQuicPendingMode と連携）
	st.OnClientConnect(func(_ transport.ClientID) {
		s.OnQUICClientConnected()
	})

	// 入力/リサイズ処理ゴルーチンを起動
	go s.handleTransportInput()
	go s.handleTransportResize()

	return nil
}

// outputFromOffset は OutputRingBuffer から offset(=Seq) >= fromOffset のチャンクを、
// 先頭から maxResyncBatchRawBytes を目安としたバッチで返す
// （transport.OnResyncNeeded の実装。再接続/長スリープ後の取りこぼし回復）。
// transport は空が返るまで「最後の offset+1」で繰り返し呼ぶ契約なので、ここで全量を
// 一括で返す必要はない（cold 層全量の一括解凍によるメモリスパイクを避ける）。
// 再接続クライアントが要求した位置より古いチャンクが evict 済みで再同期の穴を埋められない
// 場合は、QUIC 経路で再描画を促すステータスを通知する（旧 UDP の OUTPUT_DROPPED 相当）。
func (s *Session) outputFromOffset(client transport.ClientID, fromOffset uint64) ([]transport.OutputChunk, error) {
	s.mu.RLock()
	// cold 層は再接続クライアント（fromOffset>1）の欠損埋めにのみ使う。
	// 新規クライアント（fromOffset<=1）へ数日分を全再生するのは過剰なので raw 層のみ。
	var segs []coldSegment
	if fromOffset > 1 {
		for _, seg := range s.coldSegments {
			if seg.endSeq >= fromOffset {
				segs = append(segs, seg) // data は不変なのでロック外で解凍できる
			}
		}
	}
	var rawChunks []transport.OutputChunk
	for _, ch := range s.coldPending {
		if ch.Seq >= fromOffset {
			rawChunks = append(rawChunks, transport.OutputChunk{Offset: ch.Seq, Data: ch.Data})
		}
	}
	for _, ch := range s.outputChunks {
		if ch.Seq >= fromOffset {
			rawChunks = append(rawChunks, transport.OutputChunk{Offset: ch.Seq, Data: ch.Data})
		}
	}
	oldestSeq, hasAny := s.oldestRetainedSeqLocked()
	st := s.st
	s.mu.RUnlock()

	// fromOffset>1 = 既に出力を受けていた再接続クライアント。要求位置より古い側が evict
	// されている（oldestSeq > fromOffset）なら欠損が埋まらないので再描画を促す。
	// バッチ分割後の後続呼び出し（fromOffset は送信済み位置+1 まで進む）ではこの条件は
	// 成立しないため、通知は再同期 1 回につき最初の呼び出しでのみ出る。
	if st != nil && fromOffset > 1 && hasAny && oldestSeq > fromOffset {
		_ = st.SendStatus(client, "Output was dropped (use Ctrl-^ r to refresh)")
	}

	// cold 層の解凍はロック外・バッチ上限まで（遅くてよいパス。PTY reader を止めず、
	// 全セグメント分の raw を同時にメモリへ載せない）。判定はセグメント境界で行うため
	// バッチは常にセグメント単位で完結し、次バッチの fromOffset が次セグメントの
	// 先頭（または raw 層）を指す。
	var out []transport.OutputChunk
	total := 0
	for i := range segs {
		if total >= maxResyncBatchRawBytes {
			return out, nil // 続きは次バッチ（transport が offset+1 で再度呼ぶ）
		}
		chunks, err := segs[i].chunksFrom(fromOffset)
		if err != nil {
			// 壊れたセグメントはスキップ。欠損は下の gap 検出 → 再描画通知で救われる
			log.Printf("session %s: cold segment decode failed (skipping seq %d-%d): %v",
				s.ID, segs[i].startSeq, segs[i].endSeq, err)
			continue
		}
		for _, ch := range chunks {
			out = append(out, transport.OutputChunk{Offset: ch.Seq, Data: ch.Data})
			total += len(ch.Data)
		}
	}
	for _, ch := range rawChunks {
		if total >= maxResyncBatchRawBytes {
			break // 続きは次バッチ
		}
		out = append(out, ch)
		total += len(ch.Data)
	}
	return out, nil
}

// oldestRetainedSeqLocked は全層で最古の保持チャンクの Seq を返す。s.mu を保持して呼ぶこと。
func (s *Session) oldestRetainedSeqLocked() (uint64, bool) {
	if len(s.coldSegments) > 0 {
		return s.coldSegments[0].startSeq, true
	}
	if len(s.coldPending) > 0 {
		return s.coldPending[0].Seq, true
	}
	if len(s.outputChunks) > 0 {
		return s.outputChunks[0].Seq, true
	}
	return 0, false
}

// handleTransportInput は QUIC 経由の入力を PTY に転送する。
func (s *Session) handleTransportInput() {
	if s.st == nil {
		return
	}
	for {
		select {
		case <-s.done:
			return
		case input, ok := <-s.st.Input():
			if !ok {
				return
			}
			if s.debug {
				log.Printf("session %s: received input (%d bytes): %q", s.ID, len(input.Data), string(input.Data))
			}
			if err := s.WriteInput(input.Data); err != nil {
				if !s.IsPTYClosed() {
					log.Printf("session %s: failed to write input to PTY: %v", s.ID, err)
				}
			}
		}
	}
}

// handleTransportResize は QUIC 経由のリサイズ要求を処理する。
func (s *Session) handleTransportResize() {
	if s.st == nil {
		return
	}
	for {
		select {
		case <-s.done:
			return
		case resize, ok := <-s.st.Resize():
			if !ok {
				return
			}
			if err := s.Resize(resize.Rows, resize.Cols); err != nil {
				log.Printf("session %s: failed to resize PTY: %v", s.ID, err)
			}
		}
	}
}

// ptyReader reads from PTY and distributes output
func (s *Session) ptyReader() {
	defer func() {
		// PTY終了をマーク
		s.mu.Lock()
		s.ptyClosed = true
		s.ptyClosedAt = time.Now()
		s.mu.Unlock()

		// プロセスを回収して exit code を記録（通知に載せるため先に行う）
		s.reapProcess()

		// PTY終了時に全クライアントへ通知を送信
		s.notifySessionClosed()
		s.Close()
	}()

	// 64KB: read() は 1 バイトでもあれば即返るため対話レイテンシには影響せず、
	// 効くのはバースト時のみ（SendOutput のブロック中に貯まった分を少ない syscall で
	// 回収する）。Linux では単発 read が最大 32KB 程度返ることを実測済みで、
	// これ以上大きくしてもカーネル側の tty バッファが先に律速する。
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-s.done:
			return
		default:
		}

		n, err := s.ptyMaster.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("session %s: pty closed", s.ID)
			}
			return
		}

		if n > 0 {
			s.handlePTYOutput(buf[:n])
		}
	}
}

// reapProcess は PTY EOF 後にプロセスの終了を回収し、exit code を記録する。
// exit code はセッション終了通知（sessionGone / SESSION_CLOSED）に載り、
// ssh と同様にクライアントの終了コードへ伝搬される。シグナル死はシェル慣行の
// 128+signal に換算する。ここで Wait することで zombie 化も防ぐ。
// 無停止再起動で復元されたセッション（cmd なし）は proc.Wait() で回収する
// （exec は親子関係を保つので子のまま。Wait 可能）。
func (s *Session) reapProcess() {
	s.mu.RLock()
	cmd := s.cmd
	proc := s.proc
	s.mu.RUnlock()
	if proc == nil {
		return
	}
	type waitResult struct {
		ps  *os.ProcessState
		err error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		if cmd != nil {
			err := cmd.Wait()
			waitCh <- waitResult{ps: cmd.ProcessState, err: err}
			return
		}
		ps, err := proc.Wait()
		waitCh <- waitResult{ps: ps, err: err}
	}()
	var ps *os.ProcessState
	select {
	case r := <-waitCh:
		ps = r.ps
	case <-time.After(3 * time.Second):
		// PTY を閉じたままプロセスが生き続ける稀なケース。exit code は不明のままにし、
		// Close() の Kill 後に上の goroutine が回収する。
		return
	}
	code := -1
	if ps != nil {
		code = ps.ExitCode()
		if code == -1 {
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		}
	}
	s.mu.Lock()
	s.exitCode = code
	s.mu.Unlock()
}

// ExitCode returns the process's exit status (-1 = still running or
// unknown).
func (s *Session) ExitCode() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.exitCode
}

// evictOutputChunksLocked は保持上限を超えた分を hot → cold へ落とし、
// 保持期間（maxOutputBufferAge）を過ぎたものを破棄する。s.mu を保持して呼ぶこと。
// 戻り値は「圧縮待ちがセグメントサイズに達した」で、true なら呼び出し側が
// ロックを外した後に flushColdPending() を呼ぶこと（flate 圧縮をロック下で行うと
// 同じロックを取る WriteInput＝打鍵まで止めてしまうため、圧縮はロック外で行う）。
func (s *Session) evictOutputChunksLocked(now time.Time) (needColdFlush bool) {
	s.evictHotLocked(now)
	s.evictColdPendingLocked(now)
	needColdFlush = s.coldPendingBytes >= coldSegmentRawTarget && !s.coldFlushing
	s.evictColdLocked(now)
	return needColdFlush
}

// evictHotLocked: 上限超過分は cold の圧縮待ちへ移す。期限切れはそのまま破棄。
// s.mu を保持して呼ぶこと。
func (s *Session) evictHotLocked(now time.Time) {
	for len(s.outputChunks) > 0 {
		oldest := s.outputChunks[0]
		tooOld := now.Sub(oldest.Timestamp) > maxOutputBufferAge
		overLimit := len(s.outputChunks) > maxHotOutputChunks || s.outputBufferBytes > maxHotOutputBytes
		if !tooOld && !overLimit {
			break
		}
		// スライス前進では backing array 側に Data への参照が残り GC できないため、
		// ゼロ値で潰してから前進する
		s.outputChunks[0] = OutputChunk{}
		s.outputChunks = s.outputChunks[1:]
		s.outputBufferBytes -= len(oldest.Data)
		if !tooOld {
			s.coldPending = append(s.coldPending, oldest)
			s.coldPendingBytes += len(oldest.Data)
		}
	}
}

// evictColdPendingLocked: 圧縮待ち（cold 圧縮前の生チャンク）の期限切れを破棄する。
// s.mu を保持して呼ぶこと。
func (s *Session) evictColdPendingLocked(now time.Time) {
	// 圧縮中は先頭が in-flight バッチ（flushColdPending が完了時に先頭から取り除く
	// 前提）なので触らない。圧縮は数 ms で終わり、age evict は 72h 粒度なので
	// 次回に回して問題ない。
	if s.coldFlushing {
		return
	}
	for len(s.coldPending) > 0 && now.Sub(s.coldPending[0].Timestamp) > maxOutputBufferAge {
		s.coldPendingBytes -= len(s.coldPending[0].Data)
		s.coldPending[0] = OutputChunk{}
		s.coldPending = s.coldPending[1:]
	}
}

// evictColdLocked: 圧縮後サイズ・保持期間で cold セグメントを evict する。
// s.mu を保持して呼ぶこと。
func (s *Session) evictColdLocked(now time.Time) {
	for len(s.coldSegments) > 0 {
		oldest := s.coldSegments[0]
		tooOld := now.Sub(oldest.newestTime) > maxOutputBufferAge
		overLimit := s.coldBytes > maxColdOutputBytes
		if !tooOld && !overLimit {
			break
		}
		s.coldSegments[0] = coldSegment{}
		s.coldSegments = s.coldSegments[1:]
		s.coldBytes -= len(oldest.data)
		s.coldRawBytes -= oldest.rawBytes
	}
}

// flushColdPending は圧縮待ちチャンクを 1 つの coldSegment にまとめる。
// flate 圧縮（BestSpeed で 1MB あたり数 ms）はロック外で行う。圧縮中も対象チャンクは
// coldPending に残したままにするので、再同期（outputFromOffset）と handover の
// スナップショットからは常に全チャンクが見える。coldFlushing ガードで同時実行は
// 1 つに制限され（後続呼び出しは no-op）、圧縮中の追記は coldPending の末尾にのみ
// 入り、evictColdPendingLocked も先頭を触らないため、完了時は先頭 len(batch) 個を
// そのまま取り除けばよい（Data は immutable なのでロック外で読んで安全）。
func (s *Session) flushColdPending() {
	s.mu.Lock()
	if s.coldFlushing || len(s.coldPending) == 0 {
		s.mu.Unlock()
		return
	}
	s.coldFlushing = true
	batch := s.coldPending[:len(s.coldPending):len(s.coldPending)]
	s.mu.Unlock()

	seg, err := compressChunks(batch)
	batchBytes := 0
	for _, ch := range batch {
		batchBytes += len(ch.Data)
	}

	s.mu.Lock()
	for i := range batch {
		s.coldPending[i] = OutputChunk{}
	}
	s.coldPending = s.coldPending[len(batch):]
	if len(s.coldPending) == 0 {
		s.coldPending = nil
	}
	s.coldPendingBytes -= batchBytes
	if err != nil {
		// bytes.Buffer への flate 書き込みは実質失敗しないが、万一の場合は
		// この分の再同期を諦める（セーフティネットは gap → 再描画通知）
		log.Printf("session %s: cold segment compression failed (dropping %d bytes): %v",
			s.ID, batchBytes, err)
	} else {
		s.coldSegments = append(s.coldSegments, seg)
		s.coldBytes += len(seg.data)
		s.coldRawBytes += seg.rawBytes
	}
	s.coldFlushing = false
	s.mu.Unlock()
}

// EvictStaleOutput reclaims output chunks past their retention limit (called
// by the periodic cleanup). Eviction also runs on append, but a session whose
// output has stopped never triggers it, so the Manager's cleanup ticker
// covers that case. A lingering compression queue is also flushed here.
func (s *Session) EvictStaleOutput() {
	s.mu.Lock()
	now := time.Now()
	needFlush := s.evictOutputChunksLocked(now)
	if !s.coldFlushing && len(s.coldPending) > 0 && now.Sub(s.coldPending[0].Timestamp) > coldPendingFlushAge {
		needFlush = true
	}
	s.mu.Unlock()
	if needFlush {
		s.flushColdPending()
	}
}

// notifySessionClosed はセッション終了を全クライアントに通知
func (s *Session) notifySessionClosed() {
	s.mu.RLock()
	clients := make([]*Client, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	noteMsg := &proto.NoteMsg{
		Type: "NOTE",
		Kind: "SESSION_CLOSED",
		Msg:  "PTY session has ended",
	}
	if code := s.ExitCode(); code >= 0 {
		noteMsg.ExitCode = &code
	}

	noteMsgpack, err := proto.Encode(noteMsg)
	if err != nil {
		log.Printf("ERROR: session %s: failed to encode SESSION_CLOSED notification: %v", s.ID, err)
		return
	}

	// 全クライアントに通知（ブロッキングしないように）
	for _, client := range clients {
		select {
		case client.OutCh <- noteMsgpack:
		default:
			log.Printf("WARNING: session %s: failed to send SESSION_CLOSED notification to client %s", s.ID, client.ID)
		}
	}
}

// handlePTYOutput processes PTY output and sends to clients
func (s *Session) handlePTYOutput(data []byte) {
	// データが空なら何もしない
	if len(data) == 0 {
		return
	}

	// ロック下で: seq++とバッファappendのみ
	s.mu.Lock()
	s.seq++
	currentSeq := s.seq

	// デバッグ: PTY出力をログに出力
	if s.debug {
		hexPreview := hex.EncodeToString(data)
		if len(hexPreview) > 48 {
			hexPreview = hexPreview[:48] + "..."
		}
		log.Printf("session %s: handlePTYOutput seq=%d len=%d hex=%s", s.ID, currentSeq, len(data), hexPreview)
	}

	// リングバッファに保存（再接続用）
	// dataのコピーを作成（送信後に元データ（読み取りバッファ）が再利用されるため）
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)
	newChunk := OutputChunk{
		Seq:       currentSeq,
		Data:      dataCopy,
		Timestamp: time.Now(),
	}
	s.outputChunks = append(s.outputChunks, newChunk)
	s.outputBufferBytes += len(dataCopy)
	s.lastOutputAt = newChunk.Timestamp

	// 古いチャンク・バイト数超過・チャンク数超過を evict
	needColdFlush := s.evictOutputChunksLocked(time.Now())

	// クライアントリストをコピー（ロック外で送信するため）
	// QUIC のファンアウト先は quicClientIDsOut を使用
	clients := make([]*Client, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	// このセッションの QUIC クライアントへファンアウトする。識別子は (SessionID, Num) の
	// 複合 ClientID。共有 transport では別セッションの同じ Num と衝突しないよう Session で
	// 区別する。Num は RegisterQUICClient が登録した quicClientIDsOut（このセッション固有）。
	udpClients := make([]transport.ClientID, len(s.quicClientIDsOut))
	for i, num := range s.quicClientIDsOut {
		udpClients[i] = transport.ClientID{Session: s.ID, Num: num}
	}
	s.mu.Unlock()

	// cold 圧縮はロック外・別 goroutine で行う（打鍵の WriteInput だけでなく、
	// この ptyReader の出力パス自体も数 ms 止めないため）。多重起動は coldFlushing
	// ガードにより後続が no-op になるだけで安全。
	if needColdFlush {
		go s.flushColdPending()
	}

	// QUIC 経由で出力を送信（ロック外）。offset = currentSeq（セッション論理オフセット）。
	if s.st != nil && len(udpClients) > 0 {
		if err := s.st.SendOutput(currentSeq, dataCopy, udpClients); err != nil {
			log.Printf("session %s: failed to send output via QUIC: %v\n", s.ID, err)
		}
		// 再同期は OnResyncNeeded が OutputRingBuffer(offset=Seq) を引く（後続スライス）。
	}

	// UDS 経由の配信は以下のクライアントには行わない:
	// - QUIC 接続がアクティブなクライアント（QUIC が同じ内容を信頼配送するため。
	//   UDS は tezzer-ssh の ssh -L 転送で SSH の TCP に載って実ネットワークを通るので、
	//   両方に送ると WAN 帯域が実質 2 倍になる。クライアント側は renderedSeq による
	//   クロスパス重複排除を持つため、境界タイミングで両方届いても二重描画しない）
	// - BeginQuicPendingMode() による抑制中の全クライアント（DA 応答二重化の防止。
	//   蓄積分は QUIC 接続時の onResync で一括配信される）
	udsClients := clients[:0]
	if s.quicReady.Load() {
		activeQUIC := make(map[uint16]bool)
		if s.st != nil {
			for _, cid := range s.st.ActiveClients() {
				if cid.Session == s.ID {
					activeQUIC[cid.Num] = true
				}
			}
		}
		for _, client := range clients {
			if client.QUICClientID != 0 && activeQUIC[client.QUICClientID] {
				continue
			}
			udsClients = append(udsClients, client)
		}
	}
	if len(udsClients) == 0 {
		return
	}

	// OUTPUT メッセージを作成（UDS 配信対象がいるときだけ encode する）
	outputMsg := proto.OutputMsg{
		Type:      "OUTPUT",
		SessionID: s.ID,
		Seq:       currentSeq,
		Data:      data,
	}
	msgpackData, err := proto.Encode(outputMsg)
	if err != nil {
		log.Printf("session %s: failed to encode output message: %v", s.ID, err)
		return
	}

	bufferFull := false
	for _, client := range udsClients {
		select {
		case client.OutCh <- msgpackData:
		default:
			log.Printf("WARNING: session %s: client %s output channel full, dropping output seq=%d",
				s.ID, client.ID, currentSeq)
			bufferFull = true
			go s.sendOutputDroppedNotification(client.ID, currentSeq)
		}
	}
	if bufferFull {
		log.Printf("NOTE: session %s: output buffer overflow at seq=%d, consider Ctrl-L to refresh", s.ID, currentSeq)
	}
}

// sendOutputDroppedNotification sends OUTPUT_DROPPED notification to a client
func (s *Session) sendOutputDroppedNotification(clientID string, droppedSeq uint64) {
	s.mu.RLock()
	client, exists := s.clients[clientID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	noteMsg := &proto.NoteMsg{
		Type: "NOTE",
		Kind: "OUTPUT_DROPPED",
		Msg:  fmt.Sprintf("dropped output at seq=%d", droppedSeq),
	}

	noteMsgpack, err := proto.Encode(noteMsg)
	if err != nil {
		log.Printf("ERROR: session %s: failed to encode OUTPUT_DROPPED notification: %v", s.ID, err)
		return
	}

	// 通知送信を試みる（ブロッキングしないように）
	select {
	case client.OutCh <- noteMsgpack:
	default:
		// それでも詰まっている場合はログだけ残す
		log.Printf("WARNING: session %s: failed to send OUTPUT_DROPPED notification to client %s", s.ID, clientID)
	}
}

// AttachClient attaches a client to this session.
// quicClientID is the client identifier for the QUIC path (0 = QUIC unused).
func (s *Session) AttachClient(fromSeq uint64, protocol, remoteAddr string, quicClientID uint16) *Client {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.clientIDSeed++
	clientID := fmt.Sprintf("client-%d", s.clientIDSeed)

	client := &Client{
		ID:           clientID,
		Session:      s,
		OutCh:        make(chan []byte, 1000),
		Done:         make(chan struct{}),
		QUICClientID: quicClientID,
		Protocol:     protocol,
		RemoteAddr:   remoteAddr,
	}

	s.clients[clientID] = client

	// Send buffered output
	go s.sendBufferedOutput(client, fromSeq)

	return client
}

// sendBufferedOutput sends buffered output chunks to a newly attached client
func (s *Session) sendBufferedOutput(client *Client, fromSeq uint64) {
	// ロックは対象チャンクの参照コピーだけに使う。msgpack エンコードは重い処理では
	// ないがバッファが大きい（数百〜数千チャンク）と合計時間が無視できなくなるため、
	// ロック保持時間を最小化する（Data は handlePTYOutput で追記時にコピー済みの
	// immutable なバイト列なので、ロック外で参照し続けても安全）。
	s.mu.RLock()
	var chunks []OutputChunk
	for _, chunk := range s.outputChunks {
		if chunk.Seq >= fromSeq {
			chunks = append(chunks, chunk)
		}
	}
	s.mu.RUnlock()

	for _, chunk := range chunks {
		outputMsg := proto.OutputMsg{
			Type:      "OUTPUT",
			SessionID: s.ID,
			Seq:       chunk.Seq,
			Data:      chunk.Data,
		}
		msgpackData, err := proto.Encode(outputMsg)
		if err != nil {
			log.Printf("session %s: failed to encode buffered output: %v", s.ID, err)
			continue
		}
		select {
		case client.OutCh <- msgpackData:
		case <-client.Done:
			return
		}
	}

	s.RequestRedraw()
}

// RequestRedraw nudges screen/tmux to redraw by briefly changing the PTY
// size. Called after sendBufferedOutput or after a QUIC resync completes.
// Shrinking cols by one and restoring it sends SIGWINCH twice, which makes
// tmux/screen repaint. A screen-specific `screen -X redisplay` used to run in
// addition, but measurements showed GNU Screen 4.9 fully redraws on SIGWINCH
// alone, so it was removed (2026-07).
func (s *Session) RequestRedraw() {
	s.mu.RLock()
	rows, cols := s.Rows, s.Cols
	closed := s.ptyClosed
	s.mu.RUnlock()

	if !closed && cols > 1 {
		_ = s.Resize(rows, cols-1)
		time.Sleep(50 * time.Millisecond)
		_ = s.Resize(rows, cols)
	}
}

// DetachClient detaches a client from this session
func (s *Session) DetachClient(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if client, ok := s.clients[clientID]; ok {
		close(client.Done)
		delete(s.clients, clientID)

		// QUIC では接続断は transport 側が検知するため明示的な unregister は不要。

		// 最後のクライアントが切れた時刻を記録
		if len(s.clients) == 0 {
			s.lastDetachedAt = time.Now()
		}
	}
}

// WriteInput writes input to the PTY
func (s *Session) WriteInput(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// PTY が閉じられた後の Write は fd への use-after-close になる（Close() と競合）。
	// ptyClosed は s.mu 下で扱われるので、ここで弾けば Close() の fd close と直列化される
	// （Resize() と同じパターン）。
	if s.ptyClosed || s.ptyMaster == nil {
		return fmt.Errorf("PTY has been closed")
	}

	_, err := s.ptyMaster.Write(data)
	if err == nil {
		s.lastInputAt = time.Now()
	}
	return err
}

// Resize resizes the PTY
func (s *Session) Resize(rows, cols int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// PTY が閉じられた後の Setsize は fd への use-after-close になる（Close() と競合）。
	// ptyClosed は s.mu 下で扱われるので、ここで弾けば Close() の fd close と直列化される。
	if s.ptyClosed || s.ptyMaster == nil {
		return nil
	}
	if err := pty.Setsize(s.ptyMaster, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		return err
	}

	s.Rows = rows
	s.Cols = cols

	// PTYサイズ変更後、SIGWINCHを送って tmux/screen に通知
	if s.proc != nil {
		if err := s.proc.Signal(syscall.SIGWINCH); err != nil {
			if IsDebugEnabled() {
				log.Printf("session %s: failed to send SIGWINCH after resize: %v", s.ID, err)
			}
		} else {
			if IsDebugEnabled() {
				log.Printf("session %s: sent SIGWINCH after resize (%dx%d)", s.ID, cols, rows)
			}
		}
	}

	return nil
}

// Size returns the current PTY size. The Rows/Cols fields are rewritten by
// Resize() under s.mu, so reading them directly from outside the package
// without the lock is a data race; use Size for -info/-list style display.
func (s *Session) Size() (rows, cols int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Rows, s.Cols
}

// Clients returns a map of currently attached clients
func (s *Session) Clients() map[string]*Client {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]*Client, len(s.clients))
	for k, v := range s.clients {
		result[k] = v
	}
	return result
}

// GetClientCount returns the number of connected clients: the larger of the
// Unix-socket client count and the session's active QUIC client count.
func (s *Session) GetClientCount() int {
	s.mu.RLock()
	tcpCount := len(s.clients)
	st := s.st
	sessID := s.ID
	s.mu.RUnlock()

	udpCount := 0
	if st != nil {
		for _, cid := range st.ActiveClients() {
			if cid.Session == sessID {
				udpCount++
			}
		}
	}
	if udpCount > tcpCount {
		return udpCount
	}
	return tcpCount
}

// ClientInfo is the per-client information returned by GetClientInfos.
type ClientInfo struct {
	ID           string
	Protocol     string
	RemoteAddr   string
	QUICClientID uint16
}

// GetClientInfos returns information about all connected clients, including
// QUIC-only clients whose Unix-socket connection is gone.
func (s *Session) GetClientInfos() []ClientInfo {
	// ロックを保持したまま st.ActiveClients() を呼ぶとデッドロックの可能性があるため、
	// 必要な値をコピーしてからロックを解放する。
	s.mu.RLock()
	st := s.st
	sessID := s.ID
	clientsCopy := make([]*Client, 0, len(s.clients))
	for _, c := range s.clients {
		clientsCopy = append(clientsCopy, c)
	}
	s.mu.RUnlock()

	result := make([]ClientInfo, 0, len(clientsCopy))

	// TCP/UDS クライアントに紐づく QUIC ClientID を記録
	seenQUICClientNums := make(map[uint16]bool)

	for _, c := range clientsCopy {
		if c.QUICClientID != 0 {
			seenQUICClientNums[c.QUICClientID] = true
		}
		result = append(result, ClientInfo{
			ID:           c.ID,
			Protocol:     c.Protocol,
			RemoteAddr:   c.RemoteAddr,
			QUICClientID: c.QUICClientID,
		})
	}

	// QUICのみのアクティブなクライアントを追加（UDS接続が切れてQUICだけ残っているケース）
	if st != nil {
		for _, cid := range st.ActiveClients() {
			if cid.Session != sessID || seenQUICClientNums[cid.Num] {
				continue
			}
			result = append(result, ClientInfo{
				ID:           fmt.Sprintf("quic-%d", cid.Num),
				Protocol:     "QUIC",
				QUICClientID: cid.Num,
			})
		}
	}

	return result
}

// IsQUICEnabled returns whether the QUIC transport is enabled for this session
func (s *Session) IsQUICEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quicEnabled
}

// IsPTYClosed returns whether the PTY has been closed
func (s *Session) IsPTYClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ptyClosed
}

// GetPTYClosedAt returns when the PTY was closed (zero if still running)
func (s *Session) GetPTYClosedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ptyClosedAt
}

// GetLastDetachedAt returns when the last client disconnected (zero if never detached)
func (s *Session) GetLastDetachedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastDetachedAt
}

// GetLastOutputAt returns when the PTY last produced output (zero if never)
func (s *Session) GetLastOutputAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastOutputAt
}

// GetLastInputAt returns when input was last written to the PTY (zero if never)
func (s *Session) GetLastInputAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastInputAt
}

// GetBufferedOutput returns all buffered output as a single byte slice.
// Used to hand output to the client when the PTY exits immediately
// (non-interactive commands such as env).
func (s *Session) GetBufferedOutput() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.outputChunks) == 0 {
		return nil
	}

	// 全出力を結合
	totalSize := 0
	for _, chunk := range s.outputChunks {
		totalSize += len(chunk.Data)
	}

	result := make([]byte, 0, totalSize)
	for _, chunk := range s.outputChunks {
		result = append(result, chunk.Data...)
	}
	return result
}

// OutputBufferStats holds output ring buffer statistics.
type OutputBufferStats struct {
	ChunkCount      int       // chunk count in the hot tier
	TotalBytes      int       // raw bytes in the hot tier + compression queue
	ColdSegments    int       // segment count in the cold tier
	ColdBytes       int       // compressed bytes in the cold tier (approximates memory use)
	ColdRawBytes    int       // uncompressed bytes in the cold tier (approximates retained output)
	OldestChunkTime time.Time // timestamp of the oldest retained chunk across tiers
}

// GetOutputBufferStats returns statistics about the OutputRingBuffer
func (s *Session) GetOutputBufferStats() OutputBufferStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := OutputBufferStats{
		ChunkCount:   len(s.outputChunks),
		TotalBytes:   s.outputBufferBytes + s.coldPendingBytes,
		ColdSegments: len(s.coldSegments),
		ColdBytes:    s.coldBytes,
		ColdRawBytes: s.coldRawBytes,
	}

	switch {
	case len(s.coldSegments) > 0:
		stats.OldestChunkTime = s.coldSegments[0].oldestTime
	case len(s.coldPending) > 0:
		stats.OldestChunkTime = s.coldPending[0].Timestamp
	case len(s.outputChunks) > 0:
		stats.OldestChunkTime = s.outputChunks[0].Timestamp
	}

	return stats
}

// ClientSendBufferStats is per-client send health (for the -info display):
// bytes/last-send time plus the backpressure indicators
// SlowWrites/MaxWriteMs.
type ClientSendBufferStats struct {
	ClientID       uint16
	TotalBytes     int
	LastSeen       time.Time
	SlowWrites     uint64 // output writes exceeding the slow-write threshold (slow client = backpressure)
	MaxWriteMs     uint64 // largest observed write duration (ms)
	StallEpisodes  uint64 // cumulative episodes of writes blocked past the warning threshold
	CurrentStallMs uint64 // elapsed time of an in-progress stall (ms; 0 = not stalled)
	QUICRemoteAddr string // current QUIC remote address (may change with migration; empty = unknown)
	// TCP port-forwarding (-L) statistics.
	ForwardsActive         int32
	ForwardsOpened         uint64
	ForwardBytesToTarget   uint64
	ForwardBytesFromTarget uint64
}

// GetClientSendBufferStats returns per-client send health for this session.
func (s *Session) GetClientSendBufferStats() []ClientSendBufferStats {
	if s.st == nil {
		return nil
	}
	var out []ClientSendBufferStats
	for _, st := range s.st.ClientSendStats() {
		// 共有 transport では他セッションのクライアントも混じるため自セッション分に絞る。
		if st.Client.Session != s.ID {
			continue
		}
		var lastSeen time.Time
		if st.LastSendUnix != 0 {
			lastSeen = time.Unix(st.LastSendUnix, 0)
		}
		out = append(out, ClientSendBufferStats{
			ClientID:               st.Client.Num,
			TotalBytes:             int(st.BytesSent),
			LastSeen:               lastSeen,
			SlowWrites:             st.SlowWrites,
			MaxWriteMs:             st.MaxWriteMs,
			StallEpisodes:          st.StallEpisodes,
			CurrentStallMs:         st.CurrentStallMs,
			QUICRemoteAddr:         st.QUICRemoteAddr,
			ForwardsActive:         st.ForwardsActive,
			ForwardsOpened:         st.ForwardsOpened,
			ForwardBytesToTarget:   st.ForwardBytesToTarget,
			ForwardBytesFromTarget: st.ForwardBytesFromTarget,
		})
	}
	return out
}

// GetQUICPort returns the QUIC listen (UDP) port number
func (s *Session) GetQUICPort() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quicPort
}

// GetQUICKey returns the shared key for QUIC mTLS pinning
func (s *Session) GetQUICKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quicKey
}

// RegisterQUICClient registers a client as an output fan-out target. With
// QUIC the client announces clientID/SessionID in its Hello and the manager
// routes by SessionID directly, so no handshake preparation or hole punch is
// needed; this only adds the clientID to the output delivery list
// (quicClientIDsOut).
func (s *Session) RegisterQUICClient(clientID uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		return fmt.Errorf("transport not enabled")
	}
	for _, id := range s.quicClientIDsOut {
		if id == clientID {
			return nil
		}
	}
	wasEmpty := len(s.quicClientIDsOut) == 0
	s.quicClientIDsOut = append(s.quicClientIDsOut, clientID)
	// Gate 1: UDPClientInfoMsg 受信済みフラグを立てる。
	// Gate 2（QUIC onConnect）が既に揃っていれば QUIC 準備完了。
	if wasEmpty {
		s.quicUDSInfoReceived = true
		if s.quicTransportConnected {
			s.signalQuicReadyLocked()
		}
	}
	return nil
}

// OnQUICClientConnected is called when the QUIC transport has actually
// accepted a client connection. This is gate 2; if gate 1 (UDPClientInfoMsg
// received) is already satisfied it triggers StartProcess.
func (s *Session) OnQUICClientConnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quicTransportConnected {
		return
	}
	s.quicTransportConnected = true
	if s.quicUDSInfoReceived {
		s.signalQuicReadyLocked()
	}
}

// signalQuicReadyLocked は mu を保持した状態で呼ぶ。
func (s *Session) signalQuicReadyLocked() {
	s.quicReady.Store(true)
	select {
	case <-s.quicReadyCh:
	default:
		close(s.quicReadyCh)
	}
}

// BeginQuicPendingMode suppresses PTY output delivery to the Unix socket.
// The new-session flow in tezzerd calls it so DA queries and similar do not
// reach the terminal emulator via the Unix socket before both QUIC and the
// UDPClientInfoMsg have arrived.
func (s *Session) BeginQuicPendingMode() {
	s.quicReady.Store(false)
}

// QuicReadyCh returns a channel closed once both conditions (QUIC connected
// and UDPClientInfoMsg received) hold. tezzerd uses it for timeout monitoring
// and as the StartProcess trigger.
func (s *Session) QuicReadyCh() <-chan struct{} {
	return s.quicReadyCh
}

// Done returns a channel closed when the session is Close()d. Used to cancel
// waiters (the QUIC-ready timeout and similar) together with Close().
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// SetDebug toggles the session's debug output at runtime (session side only;
// the transport has no debug-flag API).
func (s *Session) SetDebug(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.debug = enabled
	// transport にはデバッグフラグ切替 API が無いため何もしない。
}

// Close closes the session.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		// 接続中クライアントへセッション消失を通知（UDS 非依存・QUIC 経路）。
		// 共有モードでは transport が残るので確実に届く。per-session では下の Close 前に送る。
		if s.st != nil {
			s.mu.RLock()
			ptyClosed := s.ptyClosed
			clients := append([]uint16(nil), s.quicClientIDsOut...)
			s.mu.RUnlock()
			reason := "NO_SUCH_SESSION: session closed"
			exitCode := -1
			if ptyClosed {
				// PTY プロセス終了由来のクローズ。クライアント側でクリーン終了させる。
				reason = "SESSION_CLOSED: PTY session has ended"
				exitCode = s.ExitCode()
			}
			for _, num := range clients {
				_ = s.st.SendSessionGone(transport.ClientID{Session: s.ID, Num: num}, reason, exitCode)
			}
		}
		// 共有 transport は manager 所有のため閉じない（per-session のときだけ閉じる）。
		if s.st != nil && !s.usesSharedTransport {
			if s.stCancel != nil {
				s.stCancel()
			}
			_ = s.st.Close()
		}
		// agent forwarding の UDS を閉じる（bind していた場合のみ）。
		if s.agentListener != nil {
			_ = s.agentListener.Close()
			_ = os.Remove(s.agentSockPath)
		}
		// ptyClosed を s.mu 下で立ててから fd を閉じる。Resize() は s.mu 下で ptyClosed を
		// 見て Setsize を弾くので、Setsize と fd close が直列化され use-after-close を防ぐ。
		s.mu.Lock()
		if !s.ptyClosed {
			s.ptyClosed = true
			s.ptyClosedAt = time.Now()
		}
		pm := s.ptyMaster
		proc := s.proc
		s.mu.Unlock()
		if pm != nil {
			pm.Close()
		}
		if proc != nil {
			proc.Kill()
		}
	})
	return nil
}
