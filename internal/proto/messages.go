package proto

import (
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// Common fields
type BaseMsg struct {
	Type string `msgpack:"type"`
	V    int    `msgpack:"v"`
	TS   int64  `msgpack:"ts,omitempty"`
}

// Client -> Server messages

type HelloMsg struct {
	Type       string `msgpack:"type"`
	V          int    `msgpack:"v"`
	ClientName string `msgpack:"client_name"`
	Cols       int    `msgpack:"cols"`
	Rows       int    `msgpack:"rows"`
}

type CreateSessionMsg struct {
	Type string            `msgpack:"type"`
	Name string            `msgpack:"name,omitempty"` // セッション名（-name。アクティブなセッション間で一意）
	Cmd  string            `msgpack:"cmd"`
	Args []string          `msgpack:"args,omitempty"`
	Env  map[string]string `msgpack:"env,omitempty"`
	Cwd  string            `msgpack:"cwd,omitempty"`
	Cols int               `msgpack:"cols"`
	Rows int               `msgpack:"rows"`
	// AgentForward は -A（SSH agent forwarding）を要求する。作成時にのみ意味を持ち、
	// セッションの寿命の間不変（docs/dev/agent-forwarding.md）。
	AgentForward bool `msgpack:"agent_forward,omitempty"`
}

type AttachSessionMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	FromSeq   uint64 `msgpack:"from_seq"`
	Cols      int    `msgpack:"cols"`
	Rows      int    `msgpack:"rows"`
	ClientID  uint16 `msgpack:"client_id,omitempty"` // クライアント識別子（UDP用）
}

type InputMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	Data      []byte `msgpack:"data"`
}

type ResizeMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	Cols      int    `msgpack:"cols"`
	Rows      int    `msgpack:"rows"`
}

type PingMsg struct {
	Type  string `msgpack:"type"`
	Nonce uint64 `msgpack:"nonce"`
}

type ListSessionsMsg struct {
	Type string `msgpack:"type"`
}

type GetSessionInfoMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

type KillSessionMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

// WaitSessionMsg は WAIT_SESSION: セッションのコマンド終了を待つ（-wait）。
// サーバは終了まで応答を保留し、終了時に NOTE (SESSION_CLOSED, exit_code 付き) を返す。
type WaitSessionMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

// UDPClientInfoMsg はクライアントの ClientID をサーバーに通知する
// （出力ファンアウト対象への登録。共有 transport モードのルーティングに必要）。
type UDPClientInfoMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	ClientID  uint16 `msgpack:"client_id"` // クライアント識別子（UDP用）
}

// GetServerMetaMsg はサーバーのメタ情報を要求
type GetServerMetaMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id,omitempty"` // セッション固有の情報を要求する場合に指定
}

// Server -> Client messages

type WelcomeMsg struct {
	Type       string `msgpack:"type"`
	ServerName string `msgpack:"server_name"`
	SessionID  string `msgpack:"session_id,omitempty"`
}

type SessionCreatedMsg struct {
	Type       string `msgpack:"type"`
	SessionID  string `msgpack:"session_id"`
	UDPEnabled bool   `msgpack:"udp_enabled,omitempty"` // UDPが有効かどうか
	UDPPort    int    `msgpack:"udp_port,omitempty"`    // UDPポート番号（サーバー側）
	UDPKey     []byte `msgpack:"udp_key,omitempty"`     // 共有鍵（32バイト、AES-256用）
	// UDPSessionID: 共有UDPモード時に使用するセッションID（8バイト）
	// 非共有モードまたは空の場合はSessionIDからハッシュを生成して使用
	UDPSessionID []byte `msgpack:"udp_session_id,omitempty"`
	PTYClosed    bool   `msgpack:"pty_closed,omitempty"` // PTYが既に終了しているか
	// InitialOutput: PTYが即座に終了した場合のバッファ出力（envコマンド等の非インタラクティブコマンド用）
	InitialOutput []byte `msgpack:"initial_output,omitempty"`
	// UDP接続アドレス情報
	STUNAddr  string `msgpack:"stun_addr,omitempty"`  // STUN経由で取得したサーバーの公開アドレス（例: "203.0.113.1:54321"）
	LocalAddr string `msgpack:"local_addr,omitempty"` // サーバーのローカルアドレス（LAN内接続用、例: "192.168.1.10"）
}

type ErrorMsg struct {
	Type    string `msgpack:"type"`
	Code    string `msgpack:"code"`
	Message string `msgpack:"message"`
}

type PongMsg struct {
	Type  string `msgpack:"type"`
	Nonce uint64 `msgpack:"nonce"`
}

// OutputMsg はPTY出力の生バイトストリームを運ぶ
type OutputMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
	Seq       uint64 `msgpack:"seq"`  // シーケンス番号（欠番検出用）
	Data      []byte `msgpack:"data"` // PTYから読んだ生データ
}

type NoteMsg struct {
	Type string `msgpack:"type"`
	Kind string `msgpack:"kind"`
	Msg  string `msgpack:"msg,omitempty"` // 追加メッセージ（OUTPUT_DROPPED等に使用）
	// ExitCode はセッションプロセスの終了コード（SESSION_CLOSED でのみ設定。
	// nil = 不明。旧サーバからの通知には付かない）。
	ExitCode *int `msgpack:"exit_code,omitempty"`
}

// ClientInfo はクライアント接続情報
type ClientInfo struct {
	ID             string   `msgpack:"id"`
	Protocol       string   `msgpack:"protocol"`                   // "UDS", "TCP", "UDP"
	RemoteAddr     string   `msgpack:"remote_addr,omitempty"`      // リモートアドレス（TCP/UDPの場合）
	QUICRemoteAddr string   `msgpack:"quic_remote_addr,omitempty"` // QUIC 接続の現在の対向アドレス（migration で変わりうる）
	UDPClientID    uint16   `msgpack:"udp_client_id,omitempty"`    // UDP クライアント識別子
	UDPAddresses   []string `msgpack:"udp_addresses,omitempty"`    // UDP接続のアドレス一覧（IP:port）
	// 送信統計（QUIC接続時のみ）
	SendBufferBytes int   `msgpack:"send_buffer_bytes,omitempty"` // 送信バイト数
	LastSeen        int64 `msgpack:"last_seen,omitempty"`         // 最後に送信/受信した時刻 (Unix秒)
	// backpressure 観測（QUIC）: 出力 Write が遅い＝遅いクライアントが PTY を詰まらせている兆候
	SlowOutputWrites uint64 `msgpack:"slow_output_writes,omitempty"`  // 出力 Write が閾値超だった回数
	MaxOutputWriteMs uint64 `msgpack:"max_output_write_ms,omitempty"` // 観測した最大 Write 所要(ms)
	// stall 観測（QUIC）: warning 水位を超えてブロックした出力 Write（進行中含む）
	OutputStallEpisodes uint64 `msgpack:"output_stall_episodes,omitempty"` // 累計エピソード数
	OutputStallMs       uint64 `msgpack:"output_stall_ms,omitempty"`       // 進行中の stall の経過(ms、0=なし)
	// TCP ポートフォワード（-L）の統計
	ForwardsActive         int    `msgpack:"forwards_active,omitempty"`           // 現在アクティブな転送接続数
	ForwardsOpened         uint64 `msgpack:"forwards_opened,omitempty"`           // 累計転送接続数
	ForwardBytesToTarget   uint64 `msgpack:"forward_bytes_to_target,omitempty"`   // client → target 累計バイト
	ForwardBytesFromTarget uint64 `msgpack:"forward_bytes_from_target,omitempty"` // target → client 累計バイト
}

type SessionInfo struct {
	SessionID   string       `msgpack:"session_id"`
	Name        string       `msgpack:"name,omitempty"` // セッション名（-name で付与、未設定なら空）
	Cmd         string       `msgpack:"cmd"`
	Rows        int          `msgpack:"rows"`
	Cols        int          `msgpack:"cols"`
	CreatedAt   int64        `msgpack:"created_at"`
	ClientCount int          `msgpack:"client_count"`
	Clients     []ClientInfo `msgpack:"clients,omitempty"` // 接続中のクライアント情報
	UDPEnabled  bool         `msgpack:"udp_enabled,omitempty"`
	UDPPort     int          `msgpack:"udp_port,omitempty"` // セッションのUDPリスニングポート
	PTYClosed   bool         `msgpack:"pty_closed,omitempty"`
	// デタッチ追跡
	LastDetachedAt int64  `msgpack:"last_detached_at,omitempty"` // 最後にクライアントが0になった時刻 (Unix秒)
	LastUDPAddr    string `msgpack:"last_udp_addr,omitempty"`    // 最後に受信した有効なUDP送信元IP
	// 活動追跡（freshness）: セッション単位の最終 PTY 出力/入力時刻 (Unix秒)。
	// クライアント接続の有無と無関係に更新される（ClientInfo.LastSeen との違い）
	LastOutputAt int64 `msgpack:"last_output_at,omitempty"`
	LastInputAt  int64 `msgpack:"last_input_at,omitempty"`
	// OutputRingBuffer 統計
	OutputChunks      int   `msgpack:"output_chunks,omitempty"`       // hot 層のチャンク数
	OutputBufferBytes int   `msgpack:"output_buffer_bytes,omitempty"` // hot 層 + 圧縮待ちの raw バイト数
	OldestChunkTime   int64 `msgpack:"oldest_chunk_time,omitempty"`   // 全層で最古のチャンクのタイムスタンプ (Unix秒)
	// cold 層（flate 圧縮された古い出力）の統計
	OutputColdSegments int `msgpack:"output_cold_segments,omitempty"`  // セグメント数
	OutputColdBytes    int `msgpack:"output_cold_bytes,omitempty"`     // 圧縮後バイト数（メモリ使用量相当）
	OutputColdRawBytes int `msgpack:"output_cold_raw_bytes,omitempty"` // 圧縮前バイト数（保持出力量相当）
}

type SessionsListMsg struct {
	Type     string        `msgpack:"type"`
	Sessions []SessionInfo `msgpack:"sessions"`
}

type SessionInfoMsg struct {
	Type    string      `msgpack:"type"`
	Session SessionInfo `msgpack:"session"`
}

type SessionKilledMsg struct {
	Type      string `msgpack:"type"`
	SessionID string `msgpack:"session_id"`
}

// ServerMetaMsg はサーバーのメタ情報を返す
type ServerMetaMsg struct {
	Type string `msgpack:"type"`
	// サーバー側全体のメタ情報
	ServerBuildID    string `msgpack:"server_build_id,omitempty"`    // サーバーのビルドID (git hash)
	ServerBuildTime  string `msgpack:"server_build_time,omitempty"`  // サーバーのビルド時刻
	ServerInstanceID []byte `msgpack:"server_instance_id,omitempty"` // サーバーインスタンスID（再起動検知用、8バイト）
}

// Encode encodes a message to msgpack bytes
func Encode(msg interface{}) ([]byte, error) {
	return msgpack.Marshal(msg)
}

// Decode decodes msgpack bytes into a message based on "type" field
func Decode(data []byte) (interface{}, error) {
	// First decode just the type field
	var base BaseMsg
	if err := msgpack.Unmarshal(data, &base); err != nil {
		return nil, err
	}

	// Then decode the full message based on type
	switch base.Type {
	case "HELLO":
		var m HelloMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "CREATE_SESSION":
		var m CreateSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "ATTACH_SESSION":
		var m AttachSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "INPUT":
		var m InputMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "RESIZE":
		var m ResizeMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "PING":
		var m PingMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "LIST_SESSIONS":
		var m ListSessionsMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "GET_SESSION_INFO":
		var m GetSessionInfoMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "KILL_SESSION":
		var m KillSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "WAIT_SESSION":
		var m WaitSessionMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "UDP_CLIENT_INFO":
		var m UDPClientInfoMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "GET_SERVER_META":
		var m GetServerMetaMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "WELCOME":
		var m WelcomeMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSION_CREATED":
		var m SessionCreatedMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "ERROR":
		var m ErrorMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "PONG":
		var m PongMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "NOTE":
		var m NoteMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSIONS_LIST":
		var m SessionsListMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSION_INFO":
		var m SessionInfoMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SESSION_KILLED":
		var m SessionKilledMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "SERVER_META":
		var m ServerMetaMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case "OUTPUT":
		var m OutputMsg
		if err := msgpack.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("unknown message type: %s", base.Type)
	}
}

// Error codes
const (
	ErrBadRequest      = "BAD_REQUEST"
	ErrNoSuchSession   = "NO_SUCH_SESSION"
	ErrInternal        = "INTERNAL"
	ErrBusy            = "BUSY"
	ErrUnauthorized    = "UNAUTHORIZED"      // 認証失敗
	ErrDuplicateName   = "DUPLICATE_NAME"    // 同名のアクティブなセッションが既に存在
	ErrTooManySessions = "TOO_MANY_SESSIONS" // アクティブなセッション数が上限（--max-sessions）に到達
)

// ValidateSessionName は -name で指定するセッション名の形式を検証する。
// クライアント・サーバ双方で使う。表示列やスクリプトでの扱いやすさのため
// 英数と . _ - のみ、63 文字までに制限する。
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("session name cannot be empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("session name too long (max 63 chars): %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("invalid session name %q (use letters, digits, '.', '_', '-')", name)
		}
	}
	return nil
}
