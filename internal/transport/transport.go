// Package transport は端末セッションのトランスポート抽象。
//
// 目的: 「入力を送り、出力を受け、複数クライアントへファンアウトし、接続の生死を観測する」
// という transport 非依存の契約だけを定義し、アプリ層（cmd/tezzer・internal/session）から
// 具体実装（QUIC = internal/qtransport）を差し替え可能にする。
//
//   - 再送 / 順序制御 / migration / 再接続 / hole punch 等の機構は実装の内側に隠す。
//   - OutputRingBuffer は「再接続後の再同期ソース」という transport 非依存の役割として残し、
//     再同期は OnResyncNeeded（offset ベース）で配線する。
package transport

import (
	"context"
	"net"
)

// ConnectionState は接続状態（transport 非依存の粒度）。
type ConnectionState int

const (
	StateConnecting ConnectionState = iota
	StateConnected
	StateRecovering // ローミング/一時断からの復帰中
	StateClosed
)

func (s ConnectionState) String() string {
	switch s {
	case StateConnecting:
		return "Connecting"
	case StateConnected:
		return "Connected"
	case StateRecovering:
		return "Recovering"
	case StateClosed:
		return "Closed"
	default:
		return "Unknown"
	}
}

// Stats は Ctrl-^ s 表示等に使う transport 非依存のスナップショット。
// 実装固有の詳細（udp の RecvBuffer 充填率、quic の輻輳窓など）は含めない。
type Stats struct {
	RTT            float64 // 直近 RTT（ms、QUIC SmoothedRTT）
	LossRate       float64 // 推定損失率（0..1、PacketsLost/PacketsSent、接続開始からの累積）
	LastRecoveryMs float64 // 直近の復帰（migration/再接続）に要した時間（ms）。skip-stale 廃止の納得材料。
	RecoveryCount  uint64  // ローミング/再接続の累計回数
	BytesSent      uint64
	BytesReceived  uint64
}

// ClientID は共有 transport 上でクライアント接続を一意に識別する。
// Session を含めることで、別セッションのクライアントが同じ Num（クライアント自己採番の
// uint16）を選んでも衝突せず、入力ルーティング/出力ファンアウトが混線しない。
type ClientID struct {
	Session string // 所属セッション ID（per-session モードでは単一）
	Num     uint16 // クライアント自己採番の識別子（認可情報ではない・routing 用）
}

// Resize は端末サイズ変更。
type Resize struct {
	Client ClientID
	Rows   int
	Cols   int
}

// Input はクライアントからの入力（サーバ側で受信）。
type Input struct {
	Client ClientID
	Data   []byte
}

// ClientTransport は端末クライアント側の抽象。
type ClientTransport interface {
	// Start は接続を確立する（quic: Dial）。確立 or 失敗まで。
	Start(ctx context.Context) error
	Close() error

	SendInput(data []byte) error
	SendResize(cols, rows int) error // 既存実装に合わせ (cols, rows) 順
	// Output はサーバ出力（順序保証）。Offset はセッション論理オフセットで、
	// 呼び出し側が UDS 経路（OutputMsg.Seq、同じ番号空間）とのクロスパス
	// 重複排除に使う。
	Output() <-chan OutputChunk

	State() ConnectionState
	OnStateChange(fn func(old, new ConnectionState, msg string))
	OnStatusMessage(fn func(msg string))
	OnLogMessage(fn func(msg string))
	OnServerMeta(fn func(buildID, buildTime string, instanceID []byte))
	// OnSessionNotFound はセッション消失通知のコールバックを登録する。
	// exitCode はセッションプロセスの終了コード（-1 = 不明。kill 等では付かない）。
	OnSessionNotFound(fn func(msg string, exitCode int))

	Stats() Stats
	// SetRoamingCandidates は STUN 等で得た到達可能アドレス候補を渡す
	// （udp: フォールバックアドレス、quic: migration 先のヒント）。
	SetRoamingCandidates(addrs []string)
	// SetCandidatesRefresher は全候補 Dial 失敗時に呼ぶ候補再生成関数を登録する。
	// STUN 再取得などを行い新しい候補リストを返す。nil を渡すと無効化。
	SetCandidatesRefresher(fn func() []string)
}

// ServerTransport は端末サーバ側の抽象（1 PTY → 複数クライアントのファンアウトを含む）。
type ServerTransport interface {
	Start(ctx context.Context) error
	Close() error

	// SendOutput は指定クライアント群へ出力をファンアウトする。
	// offset はセッション論理オフセット（Session.seq 相当、単調増加・リセットしない）。
	// transport はこれを運び、再接続後の再同期位置の基準にする。
	// 実装は offset を自分の seq 空間へ対応づける（udp: per-client SSN、quic: stream offset）。
	SendOutput(offset uint64, data []byte, clients []ClientID) error
	Input() <-chan Input
	Resize() <-chan Resize

	OnClientConnect(fn func(client ClientID))
	OnClientDisconnect(fn func(client ClientID))

	// OnResyncNeeded は、あるクライアントが fromOffset 以降の出力を要求したとき
	// （再接続・長スリープ復帰でローカルに無い分を埋める必要が出たとき）に呼ばれる。
	// session 層は OutputRingBuffer から該当オフセット以降のチャンクを返し、
	// transport がそれを当該クライアントへ再送する。これにより OutputRingBuffer は
	// session 所有のまま、SSN/フォールバックという udp 固有語を表に出さずに再同期できる。
	// コールバックは fromOffset 以降の「先頭の一部だけ」を返してよい（バッチ返却。
	// cold 層全量の一括解凍によるメモリスパイクを避けるため）。transport は空スライスが
	// 返るまで「最後に受け取った offset+1」を fromOffset にして繰り返し呼ぶこと。
	// 返るチャンクは offset 昇順・fromOffset 以上であること（呼び出しループの前進保証）。
	OnResyncNeeded(fn func(client ClientID, fromOffset uint64) ([]OutputChunk, error))

	ActiveClients() []ClientID

	// ClientSendStats はクライアントごとの出力送信健全性を返す（backpressure 観測用）。
	// 遅いクライアントへの出力 Write がブロックすると PTY reader 全体が詰まりうるため、
	// SlowWrites/MaxWriteMs で「どのクライアントが詰まらせているか」を可視化する。
	ClientSendStats() []ClientSendStat

	// SetServerMeta はクライアント接続時に通知するサーバ情報を設定する
	// （ビルド ID/時刻・インスタンス ID）。UDS に依存せず QUIC 経路で届ける。
	SetServerMeta(buildID, buildTime string, instanceID []byte)

	// SendSessionGone は指定クライアントへセッション消失を通知する（再起動/kill 検知用）。
	// UDS が死んでいても QUIC 経路で届けられるようにする。
	// exitCode はセッションプロセスの終了コード（-1 = 不明。プロセス終了由来の
	// クローズでのみ 0 以上になり、クライアントの終了コードへ伝搬される）。
	SendSessionGone(client ClientID, reason string, exitCode int) error

	// SendStatus は指定クライアントへステータスメッセージを通知する（QUIC 経路）。
	// 例: 再接続時に再同期で埋められない出力欠損（再描画を促す）。
	SendStatus(client ClientID, msg string) error

	Stats() Stats
}

// ClientSendStat はサーバ→クライアント出力の送信健全性（backpressure 観測用）。
type ClientSendStat struct {
	Client         ClientID
	BytesSent      uint64
	LastSendUnix   int64  // 最後に成功した送信時刻（Unix 秒、0=なし）
	SlowWrites     uint64 // 出力 Write が閾値超だった回数（backpressure の指標）
	MaxWriteMs     uint64 // 観測した最大 Write 所要（ms）
	StallEpisodes  uint64 // 出力 Write が warning 水位を超えてブロックした累計エピソード数
	CurrentStallMs uint64 // 進行中の stall の経過（ms、0 = 詰まっていない）
	QUICRemoteAddr string // QUIC 接続の現在の対向アドレス（migration で変わりうる。空=不明）
	// TCP ポートフォワード（-L）の per-client 統計
	ForwardsActive         int32  // 現在アクティブな転送接続数
	ForwardsOpened         uint64 // 累計転送接続数（dial 成功分）
	ForwardBytesToTarget   uint64 // client → target 方向の累計バイト
	ForwardBytesFromTarget uint64 // target → client 方向の累計バイト
}

// OutputChunk は再同期で session 層が返す、論理オフセット付きの出力片。
type OutputChunk struct {
	Offset uint64
	Data   []byte
}

// ForwardConn は TCP ポートフォワードの 1 本分の転送路（QUIC bidi ストリーム）。
// CloseWrite は送信方向だけを閉じ（FIN）、TCP の半クローズに対応させる。
// Close は両方向を閉じる。
type ForwardConn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	CloseWrite() error
	Close() error
}

// TCPForwarder は -L ポートフォワードを開ける ClientTransport のオプション機能。
// 対応実装（quic）だけが満たす。呼び出し側は型アサーションで検出する。
type TCPForwarder interface {
	// ForwardingSupported はサーバが転送対応を広告しているかを返す
	// （serverMeta 受信後に確定。未受信の間は false）。
	ForwardingSupported() bool
	// OpenForward はサーバ側から target ("host:port") へ TCP 接続する転送路を開く。
	// ctx は open ハンドシェイク（stream open + dial 応答待ち）にのみ効く。
	OpenForward(ctx context.Context, target string) (ForwardConn, error)
}

// AgentForwarder は SSH agent forwarding（-A）の中継路を開ける ServerTransport の
// オプション機能。対応実装（quic）だけが満たす。呼び出し側は型アサーションで検出する。
// -L の OpenForward とは向きが逆（サーバ側から、セッションの現在の agent provider
// クライアントへ bidi ストリームを開く）。
type AgentForwarder interface {
	// OpenAgentStream は sessionID の現在の agent provider へ中継路を開く。
	// provider が unset（-A クライアント未接続）の場合はエラーを返す。
	// ctx は open ハンドシェイクにのみ効く。
	OpenAgentStream(ctx context.Context, sessionID string) (ForwardConn, error)
}

// ---- オプション機能の named interface ----
// ServerTransport / ClientTransport 本体の契約には含めず、対応実装（quic）だけが満たす。
// 呼び出し側は型アサーションで検出する（TCPForwarder / AgentForwarder と同じパターン）。
// 無名 interface のアサーションが呼び出し側に散らばると契約の全体像が見えなくなるため、
// ここに集約して名前を付ける。

// ForwardingPolicy は転送機能の許可/禁止を設定できる ServerTransport のオプション機能
// （サーバ起動フラグ --no-tcp-forwarding / --no-agent-forwarding の適用先）。
type ForwardingPolicy interface {
	// SetTCPForwarding は TCP ポートフォワード（-L）の許可/禁止を設定する（既定: 許可）。
	SetTCPForwarding(enabled bool)
	// SetAgentForwarding は SSH agent forwarding（-A）の許可/禁止を設定する（既定: 許可）。
	SetAgentForwarding(enabled bool)
}

// SocketHandover は無停止再起動（self re-exec）に必要な fd 継承・明示切断に対応する
// ServerTransport のオプション機能。
type SocketHandover interface {
	// DupUDPSocketFd は待ち受け UDP ソケットを複製した fd（CLOEXEC なし = exec を
	// 跨いで継承される）を返す。
	DupUDPSocketFd() (int, error)
	// DisconnectAllClients は全クライアントを CONNECTION_CLOSE で即時切断する
	// （クライアントの idle timeout 待ちを回避し、即 reconnect を誘発する）。
	DisconnectAllClients(reason string)
}

// Addresser は実際の待ち受けアドレスを公開する ServerTransport のオプション機能
// （:0 bind 時のポート確定、bootstrap でクライアントへ伝える用）。
type Addresser interface {
	Addr() net.Addr
}

// ResyncSeeder は「既に他経路（UDS）で受信・描画済みの出力オフセット」を Start 前に
// 設定できる ClientTransport のオプション機能。Hello の LastOffset に反映され、
// attach 時のバックログ二重転送を防ぐ。
type ResyncSeeder interface {
	// SeedResyncOffset は Start() 前に一度だけ呼ぶこと。
	SeedResyncOffset(offset uint64)
}

// AgentForwardClient は SSH agent forwarding（-A）の ClientTransport 側オプション機能。
type AgentForwardClient interface {
	// AgentForwardingSupported はサーバが -A 対応を広告しているかを返す
	// （serverMeta 受信後に確定。未受信の間は false）。
	AgentForwardingSupported() bool
	// SetAgentSockPath は中継元のローカル agent ソケットパスを設定する
	// （Start() 前に一度だけ。空文字なら -A 無効）。
	SetAgentSockPath(path string)
}
