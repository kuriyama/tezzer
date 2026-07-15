package qtransport

// forward.go: TCP ポートフォワード（-L）の QUIC 側実装。
// 設計は docs/dev/port-forwarding.md。
//
// クライアントは TCP 接続を accept するたびに新しい bidi ストリームを開き、
// 先頭に ctrlFwdOpen{Msg: "host:port"} を送る。サーバは dial して
// ctrlFwdOpenOK / ctrlFwdOpenErr を返し、以降は生バイトの双方向中継。
// 半クローズは stream FIN ↔ TCP CloseWrite に対応させる。
// バッファリングは QUIC の per-stream flow control に委譲する。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/quic-go/quic-go"
)

const (
	// maxForwardsPerClient は 1 クライアント（QUIC 接続）あたりの同時転送接続数の上限。
	// quic-go の MaxIncomingStreams 既定（100）の内側に収める（+1 は control）。
	maxForwardsPerClient = 64
	// forwardDialTimeout はサーバ側 dial のタイムアウト。
	forwardDialTimeout = 10 * time.Second
	// forwardOpenTimeout はクライアント側の open ハンドシェイク（ctx に期限がない場合）の既定。
	forwardOpenTimeout = 15 * time.Second
)

// 転送ストリームの Cancel 時のエラーコード（デバッグ用途。プロトコル上の意味は持たせない）。
const (
	fwdCodeProtocol quic.StreamErrorCode = 1 // 先頭フレーム不正
	fwdCodeAbort    quic.StreamErrorCode = 2 // 相手側 TCP の異常切断
)

// ---- 共通（-L / -A のハンドシェイク） ----

// errRejected は開始ハンドシェイクの相手から明示的な Err 応答が返ったことを示す。
// openBidiStream の呼び出し元が errors.As で判別し、audit ログ等の分岐に使う
// （それ以外のエラー: dial/OpenStreamSync 失敗・タイムアウト等とは区別する）。
type errRejected struct{ reason string }

func (e *errRejected) Error() string { return e.reason }

// openBidiStream は bidi ストリームを開き、req を送って okType/errType の応答を待つ、
// -L（OpenForward）・-A（OpenAgentStream）共通の開始ハンドシェイク。
// ctx に期限がなければ forwardOpenTimeout を既定とする。成功時はハンドシェイク済みの
// ストリームを返す（呼び出し元が forwardStream でラップする）。
func openBidiStream(ctx context.Context, conn *quic.Conn, req *ctrlMsg, okType, errType ctrlType) (*quic.Stream, error) {
	st, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(forwardOpenTimeout)
	}
	_ = st.SetDeadline(deadline)

	fail := func(err error) (*quic.Stream, error) {
		st.CancelRead(fwdCodeAbort)
		st.CancelWrite(fwdCodeAbort)
		return nil, err
	}
	if err := writeFrame(st, req); err != nil {
		return fail(err)
	}
	m, err := readFrame(st)
	if err != nil {
		return fail(err)
	}
	if m.Type != okType {
		if m.Type == errType {
			return fail(&errRejected{reason: m.Msg})
		}
		return fail(fmt.Errorf("open: unexpected response type %d", m.Type))
	}
	_ = st.SetDeadline(time.Time{})
	return st, nil
}

// ---- サーバ側 ----

// SetTCPForwarding は転送の許可/禁止を設定する（既定: 許可）。
// transport.ServerTransport には含めず、呼び出し側が型アサーションで使う。
func (s *quicServer) SetTCPForwarding(enabled bool) {
	s.fwdDisabled.Store(!enabled)
}

// acceptForwardStreams は control 以降にクライアントが開く bidi ストリームを受ける。
// 現状の用途は転送のみ（先頭フレームで種別を確認する）。
func (s *quicServer) acceptForwardStreams(sc *serverClient) {
	for {
		st, err := sc.conn.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		go s.handleForwardStream(sc, st)
	}
}

func (s *quicServer) handleForwardStream(sc *serverClient, st *quic.Stream) {
	// 先頭フレームで種別確認。転送以外（未知の用途）は即 reset。
	_ = st.SetReadDeadline(time.Now().Add(forwardOpenTimeout))
	m, err := readFrame(st)
	_ = st.SetReadDeadline(time.Time{})
	if err != nil || m.Type != ctrlFwdOpen {
		st.CancelRead(fwdCodeProtocol)
		st.CancelWrite(fwdCodeProtocol)
		return
	}
	target := m.Msg

	reject := func(reason string) {
		_ = writeFrame(st, &ctrlMsg{Type: ctrlFwdOpenErr, Msg: reason})
		st.CancelRead(fwdCodeProtocol)
		_ = st.Close()
	}

	// --no-tcp-forwarding の二重防御（feature bit を見ないクライアント対策）。
	if s.fwdDisabled.Load() {
		reject("TCP forwarding disabled by server")
		return
	}
	if n := sc.fwdActive.Add(1); n > maxForwardsPerClient {
		sc.fwdActive.Add(-1)
		reject(fmt.Sprintf("too many forwarded connections (max %d)", maxForwardsPerClient))
		return
	}
	defer sc.fwdActive.Add(-1)

	// dial 先は常にログに残す（監査性優先。docs/dev/port-forwarding.md）。
	log.Printf("qtransport: forward: client %s/%d -> dial %s", sc.id.Session, sc.id.Num, target)

	d := net.Dialer{Timeout: forwardDialTimeout}
	conn, err := d.DialContext(s.ctx, "tcp", target)
	if err != nil {
		log.Printf("qtransport: forward: dial %s failed: %v", target, err)
		reject(err.Error())
		return
	}
	tconn := conn.(*net.TCPConn)
	if err := writeFrame(st, &ctrlMsg{Type: ctrlFwdOpenOK}); err != nil {
		_ = tconn.Close()
		st.CancelRead(fwdCodeAbort)
		st.CancelWrite(fwdCodeAbort)
		return
	}

	sc.fwdOpened.Add(1)
	forwardPipe(st, tconn, &sc.fwdBytesToTarget, &sc.fwdBytesFromTarget)
}

// halfCloseConn は forwardPipe が相手側に要求する最小の接続インタフェース。
// *net.TCPConn（-L）・*net.UnixConn（agent forwarding）の両方が満たす。
type halfCloseConn interface {
	io.Reader
	io.Writer
	io.Closer
	CloseWrite() error
}

// forwardPipe は QUIC ストリームと接続（TCP/Unix）を双方向に中継する。
// 正常な EOF は半クローズとして相手側に伝え、エラーは reset/close で伝える。
// toTarget / fromTarget には転送バイト数を計上する（-info の統計用）。
func forwardPipe(st *quic.Stream, tconn halfCloseConn, toTarget, fromTarget *atomic.Uint64) {
	done := make(chan struct{}, 2)
	go func() {
		// stream → TCP。stream FIN で TCP を半クローズ。
		n, err := io.Copy(tconn, st)
		toTarget.Add(uint64(n))
		if err == nil {
			_ = tconn.CloseWrite()
		} else {
			_ = tconn.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		// TCP → stream。TCP EOF で stream に FIN。
		n, err := io.Copy(st, tconn)
		fromTarget.Add(uint64(n))
		if err == nil {
			_ = st.Close()
		} else {
			st.CancelWrite(fwdCodeAbort)
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = tconn.Close()
}

// ---- クライアント側 ----

// ForwardingSupported はサーバが転送対応を広告しているかを返す（serverMeta 受信後に確定）。
func (c *quicClient) ForwardingSupported() bool {
	return c.serverFeatures.Load()&FeatureTCPForward != 0
}

// OpenForward はサーバ側から target へ TCP 接続する転送路を開く。
// ctx は open ハンドシェイクにのみ効く（確立後の中継には影響しない）。
func (c *quicClient) OpenForward(ctx context.Context, target string) (transport.ForwardConn, error) {
	if !c.ForwardingSupported() {
		return nil, errors.New("server does not advertise TCP forwarding (old server or --no-tcp-forwarding)")
	}
	c.migrateMu.Lock()
	conn := c.conn
	c.migrateMu.Unlock()
	if conn == nil || conn.Context().Err() != nil {
		return nil, errors.New("not connected")
	}

	st, err := openBidiStream(ctx, conn, &ctrlMsg{Type: ctrlFwdOpen, Msg: target}, ctrlFwdOpenOK, ctrlFwdOpenErr)
	if err != nil {
		var rej *errRejected
		if errors.As(err, &rej) {
			return nil, fmt.Errorf("forward rejected by server: %s", rej.reason)
		}
		return nil, fmt.Errorf("forward open: %w", err)
	}
	return &forwardStream{st: st}, nil
}

// forwardStream は quic.Stream を transport.ForwardConn に適合させる。
type forwardStream struct{ st *quic.Stream }

func (f *forwardStream) Read(p []byte) (int, error)  { return f.st.Read(p) }
func (f *forwardStream) Write(p []byte) (int, error) { return f.st.Write(p) }

// CloseWrite は送信方向だけを閉じる（FIN）。受信は継続できる。
func (f *forwardStream) CloseWrite() error { return f.st.Close() }

// Close は両方向を閉じる。
func (f *forwardStream) Close() error {
	f.st.CancelRead(fwdCodeAbort)
	return f.st.Close()
}

var _ transport.TCPForwarder = (*quicClient)(nil)
