package main

// client.go: クライアントの状態（Client 型）と端末 UI まわり。
// ステータス行・通知ログ・RTT スパークライン・エスケープキー操作（status/help/
// kill/redraw 等）・リサイズ送信・終了コードの追跡。

import (
	"fmt"
	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/termui"
	"github.com/kuriyama/tezzer/internal/transport"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// logEntry は通知メッセージのログエントリ
type logEntry struct {
	timestamp time.Time
	message   string
}

// maxLogEntries は保持する通知メッセージの最大数
const maxLogEntries = 20

// Client はクライアント状態を保持
type Client struct {
	conn      net.Conn
	sessionID string
	termFd    int
	width     int
	height    int
	done      chan struct{}
	doneOnce  sync.Once // doneチャネルを1回だけcloseするため
	// QUIC トランスポート（nil の場合は UDS のみ）。startQUICTransport（メイン goroutine）が
	// 書き、handleServer/handleStdin など別 goroutine が読むため ctMu で保護する。
	ct   transport.ClientTransport
	ctMu sync.RWMutex

	escapeByte    byte          // エスケープキーのバイト値
	ipv4Only      bool          // IPv4のみ使用する場合true
	escapePressed bool          // エスケープキーが押されたかどうか
	lastSeq       atomic.Uint64 // UDS 経路で最後に受信したシーケンス番号（欠番検出用）
	// renderedSeq は stdout へ描画済みの最大セッションオフセット。UDS の OutputMsg.Seq と
	// QUIC の出力 offset は同じ番号空間（サーバの currentSeq）なので、このカウンタ 1 つで
	// クロスパスの重複排除ができる（attach 時のバックログ二重描画の防止と、
	// QUIC 断中に UDS 経由の出力を安全に描画するためのフォールバック）。
	// 更新は renderMu 下で行う（読み取りはどこからでも可）。
	renderedSeq atomic.Uint64
	renderMu    sync.Mutex // renderedSeq の判定〜stdout 書き込みの直列化（UDS/QUIC 両経路）
	statusMgr   *termui.StatusManager

	lastOutputRecv atomic.Int64 // 最後に PTY 出力を受信した Unix ナノ秒（0 = 未受信）

	// RTT 履歴（Ctrl-^ i のスパークライン表示用、直近 rttHistorySize 件・古い順）
	rttHistoryMu sync.Mutex
	rttHistory   []float64

	errCh                 chan error  // 異常終了通知用（バッファ1）
	killing               atomic.Bool // Ctrl-^ q による kill 処理中（sessionGone をエラー扱いしない）
	sessionClosedNotified atomic.Bool // SESSION_CLOSED 表示済みフラグ（UDS/QUIC どちらが先着しても一度だけ表示）
	// セッションプロセスの終了コード（-1 = 未受信）。SESSION_CLOSED 通知
	//（QUIC/UDS どちらの経路でも）で設定され、ssh と同様にクライアント自身の
	// 終了コードになる。
	sessionExitCode atomic.Int64

	// 動的デバッグ出力制御
	debugEnabled atomic.Value // bool を格納

	// サーバーメタ情報
	serverMeta *proto.ServerMetaMsg
	metaMu     sync.RWMutex

	// 通知メッセージログ（Ctrl-^ s 表示用）
	msgLogMu sync.RWMutex
	msgLog   []logEntry // 最新20件のログ

	// クライアントログファイル（常時書き出し）
	logFile *os.File
	fileLog *fileLogger

	udsAddr string     // UDS接続先アドレス
	connMu  sync.Mutex // conn更新時のロック

	// 初期ステータスメッセージ（接続直後に表示）
	initialStatusMsg string

	// ESC batching用（カーソルキー等の複数バイト入力をまとめる）
	escBatchActive bool
	escBatchBuf    []byte
	escBatchStart  time.Time

	// 通常入力batching用（ペースト時にまとめて送信）
	inputBatchBuf   []byte
	inputBatchStart time.Time

	// -L ポートフォワード
	forwards    []forwardSpec // 解析済みの -L 指定（不変）
	fwdActive   atomic.Int32  // アクティブな転送 TCP 接続数（Ctrl-^ i 表示用）
	fwdWarnMu   sync.Mutex
	lastFwdWarn time.Time // ステータス行への警告の間引き用

	// -N: 転送専用 attach（端末を触らない。PTY 出力は破棄、statusMgr は nil）
	noPTY bool

	// -peek: 読み取り専用 attach（誤爆防止）。入力・リサイズ・kill を一切送らない。
	// 出力表示とエスケープキー操作（detach/status 等）はそのまま使える
	readOnly bool
	roWarned bool // 入力破棄の初回通知済み（handleStdin goroutine 内でのみ触る）

	// -A: SSH agent forwarding。解決済みのローカル $SSH_AUTH_SOCK パス（不変。
	// 空文字なら -A 未指定 or ソケット不可＝Hello の AgentForward は false）。
	agentSockPath string
}

// exitCode はクライアントの終了コードを返す。セッションプロセスの終了コードを
// 受信していればそれ（ssh と同じ挙動）、未受信（detach 等）なら 0。
func (c *Client) exitCode() int {
	if v := c.sessionExitCode.Load(); v >= 0 {
		return int(v)
	}
	return 0
}

// noteSessionExitCode はセッション終了通知に載ってきた終了コードを記録する。
// 自分が kill した場合（Ctrl-^ q）は意図した終了なので 0 のままにする。
func (c *Client) noteSessionExitCode(code int) {
	if code >= 0 && !c.killing.Load() {
		c.sessionExitCode.Store(int64(code))
	}
}

// transport は現在の QUIC トランスポートを返す（nil の場合は UDS のみ）。ctMu で保護。
func (c *Client) transport() transport.ClientTransport {
	c.ctMu.RLock()
	defer c.ctMu.RUnlock()
	return c.ct
}

// setTransport は QUIC トランスポートを設定する。
func (c *Client) setTransport(t transport.ClientTransport) {
	c.ctMu.Lock()
	c.ct = t
	c.ctMu.Unlock()
}

const (
	rttHistorySize    = 10
	rttSampleInterval = 3 * time.Second
)

// sampleRTTHistory は Ctrl-^ i のスパークライン表示用に RTT を定期サンプリングする。
// トランスポート未確立中（UDS のみ）はスキップする。
func (c *Client) sampleRTTHistory() {
	ticker := time.NewTicker(rttSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			ct := c.transport()
			if ct == nil {
				continue
			}
			rtt := ct.Stats().RTT
			c.rttHistoryMu.Lock()
			c.rttHistory = append(c.rttHistory, rtt)
			if len(c.rttHistory) > rttHistorySize {
				c.rttHistory = c.rttHistory[len(c.rttHistory)-rttHistorySize:]
			}
			c.rttHistoryMu.Unlock()
		}
	}
}

var sparklineBlocks = []rune("▁▂▃▄▅▆▇█")

// renderSparkline は RTT サンプル列をウィンドウ内 min/max 正規化した
// ブロック文字列に変換する（直近の遅延変動を一目で見るため）。
func renderSparkline(samples []float64) string {
	if len(samples) == 0 {
		return ""
	}
	lo, hi := samples[0], samples[0]
	for _, v := range samples {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	span := hi - lo
	out := make([]rune, len(samples))
	for i, v := range samples {
		if span == 0 {
			out[i] = sparklineBlocks[0]
			continue
		}
		idx := int((v - lo) / span * float64(len(sparklineBlocks)-1))
		out[i] = sparklineBlocks[idx]
	}
	return string(out)
}

// showStatus displays status information for 3 seconds
func (c *Client) showStatus() {
	sessID := c.sessionID
	if len(sessID) > 6 {
		sessID = sessID[:6]
	}
	transportInfo := "UDS"
	if ct := c.transport(); ct != nil {
		rtt := ct.Stats().RTT
		c.rttHistoryMu.Lock()
		spark := renderSparkline(c.rttHistory)
		c.rttHistoryMu.Unlock()
		transportInfo = fmt.Sprintf("%s RTT:%.0fms %s", ct.State(), rtt, spark)
	}
	outInfo := ""
	if t := c.lastOutputRecv.Load(); t != 0 {
		elapsed := time.Since(time.Unix(0, t)).Round(time.Second)
		outInfo = fmt.Sprintf(" | out:%s", elapsed)
	}
	fwdInfo := ""
	if len(c.forwards) > 0 {
		fwdInfo = fmt.Sprintf(" | fwd:%d/%d", c.fwdActive.Load(), len(c.forwards))
	}
	status := fmt.Sprintf("[Tezzer] sess:%s | %s%s%s", sessID, transportInfo, outInfo, fwdInfo)
	c.setStatusMessage(status)
}

func (c *Client) showHelp() {
	escKey := escapeKeyDisplay(c.escapeByte)
	help := fmt.Sprintf("[Tezzer] %s+ .Detach qQuit rRedraw fForce iStatus sStats dDebug hHelp", escKey)
	c.setStatusMessage(help)
}

// killSession はセッションを終了し、クライアントを終了する
func (c *Client) killSession() {
	fmt.Fprintf(os.Stderr, "\033[?25h")

	// kill 中フラグを先に立てる（QUIC sessionGone がエラー扱いされないように）
	c.killing.Store(true)

	killMsg := proto.KillSessionMsg{
		Type:      "KILL_SESSION",
		SessionID: c.sessionID,
	}
	killData, err := proto.Encode(killMsg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nError encoding kill message: %v\r\n", err)
		c.forceExit()
		return
	}

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		fmt.Fprintf(os.Stderr, "\r\nSession %s: connection not available\r\n", c.sessionID)
		c.forceExit()
		return
	}

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := netx.WriteFrame(conn, killData); err != nil {
		fmt.Fprintf(os.Stderr, "\r\nFailed to send kill: %v\r\n", err)
		c.forceExit()
		return
	}

	// UDS 応答は待たない。QUIC の sessionGone が確認経路。
	fmt.Fprintf(os.Stderr, "\r\nSession %s killed.\r\n", c.sessionID)
	c.forceExit()
}

// forceExit は強制的にクライアントを終了する（killSession用ヘルパー）
func (c *Client) forceExit() {
	// 終了処理
	select {
	case c.errCh <- errDetached:
	default:
	}
	c.doneOnce.Do(func() { close(c.done) })

	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMu.Unlock()
}

// resetScrollRegion はスクロール領域をリセットし、画面をリフレッシュする
func (c *Client) resetScrollRegion() {
	// スクロール領域をデフォルト（全画面）にリセット
	// \e[r = DECSTBM（Set Top and Bottom Margins）パラメータなしでデフォルト
	fmt.Fprintf(os.Stderr, "\033[r")

	// 画面をクリアして再描画を促す
	// \e[2J = 画面全体をクリア、\e[H = カーソルをホームポジションへ
	fmt.Fprintf(os.Stderr, "\033[2J\033[H")

	c.setStatusMessage("Scroll region reset")
}

// requestServerRedraw はサーバー側 PTY に resize trick で再描画を促す。
// QUIC resync 後（OutputRingBuffer の replay が発生したケース）に呼ぶ。
// TCP reattach は session.RequestRedraw がサーバー側で直接処理する。
func (c *Client) requestServerRedraw() {
	go func() {
		time.Sleep(300 * time.Millisecond)
		w, h, err := term.GetSize(c.termFd)
		if err != nil || w <= 1 {
			return
		}
		c.sendResize(w-1, h)
		time.Sleep(50 * time.Millisecond)
		// 待機中に本物の SIGWINCH resize が入っている可能性があるため、
		// 縮小前のサイズを使い回さず、端末の現在サイズを取り直して復元する。
		w, h, err = term.GetSize(c.termFd)
		if err != nil {
			return
		}
		c.sendResize(w, h)
	}()
}

// setStatusMessage はステータスメッセージをログに追加し、3秒間表示する
func (c *Client) setStatusMessage(msg string) {
	c.addLogMessage(msg)
	if c.statusMgr == nil {
		// -N（転送専用）ではステータス行がないので通常のログに出す
		log.Print(msg)
		return
	}
	c.statusMgr.Set(msg)
}

// addLogMessage は通知メッセージをログバッファに追加し、ログファイルにも書き出す
func (c *Client) addLogMessage(msg string) {
	c.msgLogMu.Lock()
	defer c.msgLogMu.Unlock()

	entry := logEntry{
		timestamp: time.Now(),
		message:   msg,
	}

	c.msgLog = append(c.msgLog, entry)

	// 最大件数を超えたら古いものを削除
	if len(c.msgLog) > maxLogEntries {
		c.msgLog = c.msgLog[len(c.msgLog)-maxLogEntries:]
	}

	// ファイルにも書き出す（minimal ログ）
	c.fileLog.Print(msg)
}

// getLogMessages はログメッセージのコピーを返す
func (c *Client) getLogMessages() []logEntry {
	c.msgLogMu.RLock()
	defer c.msgLogMu.RUnlock()

	// コピーを返す
	result := make([]logEntry, len(c.msgLog))
	copy(result, c.msgLog)
	return result
}

// renderOutput は出力チャンク列を重複排除しつつ stdout へ描画する。
// UDS（OutputMsg.Seq）と QUIC（出力 offset）は同じ番号空間（チャンクごとに +1）なので、
// 描画済み最大オフセット（renderedSeq）1 つでクロスパスの一度だけ描画を保証できる。
//
// fromQUIC = true（信頼・順序配送）は frontier からのジャンプを許す（リングバッファ
// evict 由来の正当な欠落。別途 redraw を促すステータス通知が届く）。
// fromQUIC = false（UDS。サーバ側 OutCh あふれで欠落しうる）は、QUIC 併用時は
// 厳密連続（frontier+1）のみ描画し、欠落を跨いだチャンクは QUIC 側の信頼配送に
// 任せて捨てる。QUIC 未確立（UDS 単独）のときは従来どおり欠落を跨いで描画する。
func (c *Client) renderOutput(chunks []transport.OutputChunk, fromQUIC bool) {
	c.renderMu.Lock()
	defer c.renderMu.Unlock()
	frontier := c.renderedSeq.Load()
	var buf []byte
	for _, ch := range chunks {
		switch {
		case ch.Offset <= frontier:
			// 描画済み（他経路が先行）
		case ch.Offset == frontier+1 || fromQUIC || c.transport() == nil:
			buf = append(buf, ch.Data...)
			frontier = ch.Offset
		default:
			// UDS 経路の欠落を跨いだチャンク: QUIC が同じ内容を信頼配送するので捨てる
		}
	}
	if len(buf) == 0 {
		return
	}
	c.renderedSeq.Store(frontier)
	c.writeOutput(buf)
}

func (c *Client) writeOutput(data []byte) {
	c.lastOutputRecv.Store(time.Now().UnixNano())
	if c.noPTY {
		// -N: 出力は表示しない。ただし読み捨ては必要（読まないとサーバ側の
		// 出力 Write が詰まり、他クライアントや PTY reader に波及する）。
		return
	}
	os.Stdout.Write(data)
}

// isDebugEnabled はデバッグ出力が有効かどうかを返す
func (c *Client) isDebugEnabled() bool {
	v := c.debugEnabled.Load()
	if v == nil {
		return false
	}
	return v.(bool)
}

// toggleDebug はデバッグ出力のON/OFFをトグルする
func (c *Client) toggleDebug() {
	oldValue := c.isDebugEnabled()
	newValue := !oldValue
	c.debugEnabled.Store(newValue)

	// debug ON/OFF に応じて log.SetOutput をファイルにティーするか切り替える
	stderrWriter := &termui.CRLFWriter{W: os.Stderr}
	if newValue && c.logFile != nil {
		log.SetOutput(io.MultiWriter(stderrWriter, c.logFile))
	} else {
		log.SetOutput(stderrWriter)
	}

	// ステータスメッセージで通知
	status := "OFF"
	if newValue {
		status = "ON"
	}
	c.setStatusMessage(fmt.Sprintf("[Tezzer] Debug output: %s", status))
}

// sendResize sends a resize message to the server
func (c *Client) sendResize(width, height int) error {
	// -peek: リモート PTY のサイズを変えない（他クライアントと本体の表示を乱さない）
	if c.readOnly {
		return nil
	}
	c.width = width
	c.height = height

	// QUIC トランスポートがある場合はそちらで送信
	if ct := c.transport(); ct != nil {
		if err := ct.SendResize(width, height); err != nil {
			// スリープ復帰直後など、再接続完了前に resize が飛ぶと一時的に失敗しうる
			// （requestServerRedraw が再接続後に現在サイズを送り直すため実害はない）。
			// 生の log.Printf は raw mode の端末内容に直接混ざるため、他の一時的な
			// ネットワーク断メッセージと同様 setStatusMessage 経由にする。
			c.setStatusMessage(fmt.Sprintf("QUIC send resize error: %v", err))
		}
		return nil
	}

	// TCP経由で送信
	resizeMsg := proto.ResizeMsg{
		Type:      "RESIZE",
		SessionID: c.sessionID,
		Cols:      width,
		Rows:      height,
	}
	resizeData, err := proto.Encode(resizeMsg)
	if err != nil {
		return fmt.Errorf("encode resize error: %w", err)
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	if err := netx.WriteFrame(c.conn, resizeData); err != nil {
		return fmt.Errorf("write resize error: %w", err)
	}
	return nil
}

// restoreTerminalFlags はターミナルフラグを復元する（ONLCR等）
// プラットフォーム固有の実装は terminal_*.go に定義されている
