package qtransport

// control ストリーム（bidi）上の制御メッセージの framing。
// 4 バイト長プレフィックス + msgpack。Hello（接続→clientID 紐付け）と Resize を運ぶ。
// 出力/入力のバルクは別の uni stream で生バイトとして流すので、ここは小さい制御のみ。

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/vmihailenco/msgpack/v5"
)

type ctrlType uint8

const (
	ctrlHello       ctrlType = 1 // クライアント → サーバ: 接続を clientID へ紐付け
	ctrlResize      ctrlType = 2 // クライアント → サーバ: 端末サイズ変更
	ctrlServerMeta  ctrlType = 3 // サーバ → クライアント: ビルド情報・インスタンスID
	ctrlSessionGone ctrlType = 4 // サーバ → クライアント: セッション消失（再起動/kill）
	ctrlStatus      ctrlType = 5 // サーバ → クライアント: ステータスメッセージ
	ctrlLog         ctrlType = 6 // サーバ → クライアント: ログメッセージ

	// 以下は control ストリームではなく、転送用 bidi ストリームの先頭でだけ使う
	// （forward.go 参照。framing は writeFrame/readFrame を共用）。
	ctrlFwdOpen    ctrlType = 7 // クライアント → サーバ: 転送開始要求（Msg = dial 先 "host:port"）
	ctrlFwdOpenOK  ctrlType = 8 // サーバ → クライアント: dial 成功、以降は生バイトの中継
	ctrlFwdOpenErr ctrlType = 9 // サーバ → クライアント: 拒否/dial 失敗（Msg = 理由）

	// SSH agent forwarding（-A）用。forwardOpen 系と対称だが向きが逆
	// （サーバ → クライアントへ bidi ストリームを開く）。agent.go 参照。
	ctrlAgentOpen    ctrlType = 10 // サーバ → クライアント: agent dial 要求
	ctrlAgentOpenOK  ctrlType = 11 // クライアント → サーバ: dial 成功、以降は生バイトの中継
	ctrlAgentOpenErr ctrlType = 12 // クライアント → サーバ: 拒否/dial 失敗（Msg = 理由）
)

// FeatureTCPForward は ctrlServerMeta.Features のビット: TCP ポートフォワード対応。
// サーバが --no-tcp-forwarding で起動された場合は立たない。
const FeatureTCPForward uint64 = 1 << 0

// FeatureAgentForward は ctrlServerMeta.Features のビット: SSH agent forwarding（-A）対応。
// サーバが --no-agent-forwarding で起動された場合は立たない。
const FeatureAgentForward uint64 = 1 << 1

const maxCtrlFrame = 1 << 20    // 1 MiB（制御メッセージなので十分すぎる上限）
const maxOutputFrame = 16 << 20 // 16 MiB（1 PTY 読み取りチャンクの上限）

// maxDupInputSize は DATAGRAM で二重送信する入力サイズの上限。
// これ以下＝打鍵（エコー遅延が体感に直結）だけが対象になり、ペーストは
// ストリームのみになる。DATAGRAM は 1 UDP パケットに収まる必要があるため
// InitialPacketSize=1200 の実効ペイロード（~1.1KB）にも収まる値にする。
const maxDupInputSize = 512

type ctrlMsg struct {
	Type         ctrlType `msgpack:"t"`
	ClientID     uint16   `msgpack:"c,omitempty"`
	SessionID    string   `msgpack:"s,omitempty"` // Hello: 所属セッション ID（共有 transport の routing 用）
	Cols         int      `msgpack:"x,omitempty"`
	Rows         int      `msgpack:"y,omitempty"`
	LastOffset   uint64   `msgpack:"o,omitempty"`  // Hello: クライアントが最後に受信した出力 offset（再同期用）
	AgentForward bool     `msgpack:"af,omitempty"` // Hello: -A 付き起動 かつ ローカル $SSH_AUTH_SOCK が使える場合のみ true
	// サーバ → クライアント
	BuildID    string `msgpack:"b,omitempty"`  // ServerMeta: ビルド ID
	BuildTime  string `msgpack:"bt,omitempty"` // ServerMeta: ビルド時刻
	InstanceID []byte `msgpack:"i,omitempty"`  // ServerMeta: サーバインスタンス ID
	Features   uint64 `msgpack:"f,omitempty"`  // ServerMeta: 機能ビット（FeatureTCPForward 等）
	Msg        string `msgpack:"m,omitempty"`  // SessionGone/Status/Log の本文、FwdOpen の dial 先、FwdOpenErr の理由
	// ExitCode はセッションプロセスの終了コード（SessionGone でのみ設定。
	// nil = 不明。ポインタなのは exit 0 と未設定を区別するため。旧相手では単に無視される）。
	ExitCode *int32 `msgpack:"ec,omitempty"`
}

// 出力ストリームのフレーム: [offset:8][len:4][data]。
// offset はセッション論理オフセット（OutputChunk.Seq）。クライアントはこれで
// 「最後に受けた offset」を追跡し、再接続時の再同期と重複排除に使う。
// ヘッダとデータは 1 回の Write で書く。QUIC stream への Write を分けると、
// パッカーが間に走った場合にヘッダだけの小さいパケットが先行しうるため。
func writeOutputFrame(w io.Writer, offset uint64, data []byte) error {
	buf := make([]byte, 12+len(data))
	binary.BigEndian.PutUint64(buf[0:8], offset)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(data)))
	copy(buf[12:], data)
	_, err := w.Write(buf)
	return err
}

func readOutputFrame(r io.Reader) (offset uint64, data []byte, err error) {
	var hdr [12]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	offset = binary.BigEndian.Uint64(hdr[0:8])
	n := binary.BigEndian.Uint32(hdr[8:12])
	if n > maxOutputFrame {
		return 0, nil, errors.New("qtransport: output frame too large")
	}
	data = make([]byte, n)
	if _, err = io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return offset, data, nil
}

// 入力 datagram: [offset:8][data]。小さい入力（打鍵）の投機的二重送信に使う。
// offset は入力ストリーム上の累計送信バイト位置（接続単位。再接続でリセット）。
// ストリーム側が正で、datagram はロス時に先に届けば得なだけのベストエフォート。
func encodeInputDatagram(offset uint64, data []byte) []byte {
	buf := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(buf[0:8], offset)
	copy(buf[8:], data)
	return buf
}

func decodeInputDatagram(b []byte) (offset uint64, data []byte, ok bool) {
	if len(b) < 8 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint64(b[0:8]), b[8:], true
}

// writeOutputFrame と同様、長さプレフィックスと本体を 1 回の Write で書く。
func writeFrame(w io.Writer, m *ctrlMsg) error {
	b, err := msgpack.Marshal(m)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

func readFrame(r io.Reader) (*ctrlMsg, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxCtrlFrame {
		return nil, errors.New("qtransport: control frame too large")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	var m ctrlMsg
	if err := msgpack.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
