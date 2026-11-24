package qtransport

// rforward.go: リモートポートフォワード（-R）の QUIC 側実装。
// 設計は docs/dev/port-forwarding.md の「Phase 2 設計: `-R`」。
//
// サーバ側の listener はこの層では持たない（tezzerd 全体の資源なので session.Manager が
// 持つ）。ここが担うのは次の 3 つだけ:
//   - Hello の DeviceID / RemoteForwards を上位へ渡す（OnRemoteForward の hello）
//   - 接続終了を「明示的終了 / 接続喪失」に分けて上位へ渡す（同 gone）
//   - 上位が accept した着信ごとに、指定クライアントへ bidi ストリームを開く
//     （OpenRemoteForward。先頭に ctrlRFwdOpen{Port}）
// クライアントは Port から自分の -R 定義の dial 先を引いて dial し、
// ctrlRFwdOpenOK / ctrlRFwdOpenErr を返す。以降は forwardPipe で中継する。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"slices"
	"sync/atomic"

	"github.com/quic-go/quic-go"
)

// ---- サーバ側 ----

// OnRemoteForward registers the remote forwarding (-R) callbacks and enables
// advertising -R support; until then Hello's RemoteForwards are ignored. The
// listening side lives above the transport (the session manager owns the
// server-side listeners). hello is called whenever a client that requests
// remote forwards says Hello (the first connect and every reconnect). gone is
// called when such a client's connection ends and no newer connection has
// replaced it; explicit reports that the client closed it on purpose
// (detach, exit) rather than being lost (sleep, network failure).
func (s *Server) OnRemoteForward(hello func(ClientID, string, []uint16), gone func(id ClientID, explicit bool)) {
	s.mu.Lock()
	s.rfwdHello = hello
	s.rfwdGone = gone
	s.mu.Unlock()
}

// remoteForwardEnabled は -R を受け付けるか（コールバック登録済み かつ 転送が許可）。
func (s *Server) remoteForwardEnabled() bool {
	s.mu.Lock()
	ok := s.rfwdHello != nil
	s.mu.Unlock()
	return ok && !s.fwdDisabled.Load()
}

// maxDeviceIDLen は Hello の DeviceID として受け付ける長さの上限
// （正規のクライアントは 32 文字。制御フレーム上限まで詰めた値をメモリに抱えないため）。
const maxDeviceIDLen = 64

// rfwdHelloFor は Hello が -R を要求していればコールバックを返す（要求なし・無効なら nil）。
func (s *Server) rfwdHelloFor(m *ctrlMsg) func(ClientID, string, []uint16) {
	if len(m.RemoteForwards) == 0 || m.DeviceID == "" || s.fwdDisabled.Load() {
		return nil
	}
	if len(m.DeviceID) > maxDeviceIDLen {
		log.Printf("qtransport: client %d (session %s): -R ignored: device ID too long (%d bytes)", m.ClientID, m.SessionID, len(m.DeviceID))
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rfwdHello
}

// closedByClientExit はクライアントが意図して接続を閉じた（connCodeClientExit）かを返す。
func closedByClientExit(conn *quic.Conn) bool {
	var ae *quic.ApplicationError
	return errors.As(context.Cause(conn.Context()), &ae) && ae.Remote && ae.ErrorCode == connCodeClientExit
}

// OpenRemoteForward opens a relay channel to client for a connection
// accepted on the server-side port. The client dials its own configured
// target for that port. An explicit refusal (for example, the client's dial
// failed) wraps ErrForwardRejected. ctx applies only to the open
// handshake.
func (s *Server) OpenRemoteForward(ctx context.Context, client ClientID, port uint16) (ForwardConn, error) {
	if s.fwdDisabled.Load() {
		return nil, errors.New("TCP forwarding disabled by server")
	}
	s.mu.Lock()
	sc := s.clients[client]
	s.mu.Unlock()
	if sc == nil {
		return nil, fmt.Errorf("client %d (session %s) not connected", client.Num, client.Session)
	}
	st, err := openBidiStream(ctx, sc.conn, &ctrlMsg{Type: ctrlRFwdOpen, Port: port}, ctrlRFwdOpenOK, ctrlRFwdOpenErr)
	if err != nil {
		var rej *errRejected
		if errors.As(err, &rej) {
			return nil, fmt.Errorf("%w: %s", ErrForwardRejected, rej.reason)
		}
		return nil, fmt.Errorf("remote forward open: %w", err)
	}
	return &forwardStream{st: st}, nil
}

// ---- クライアント側 ----

// RemoteForwardingSupported reports whether the server advertises -R support
// (known once serverMeta has been received; false until then).
func (c *Client) RemoteForwardingSupported() bool {
	return c.serverFeatures.Load()&FeatureRemoteForward != 0
}

// RemoteForwardActive returns the number of relayed -R connections in
// progress.
func (c *Client) RemoteForwardActive() int {
	return int(c.rfwdActive.Load())
}

// remoteForwardPorts は Hello に載せる -R のポート一覧（昇順）を返す。
func (c *Client) remoteForwardPorts() []uint16 {
	if len(c.rfwdTargets) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(c.rfwdTargets))
}

// helloMsg は初回接続・reconnect で送る Hello を組み立てる。
func (c *Client) helloMsg() *ctrlMsg {
	m := &ctrlMsg{Type: ctrlHello, ClientID: c.clientID, SessionID: c.sessionID, LastOffset: c.lastOffset.Load(), AgentForward: c.agentSockPath != ""}
	if ports := c.remoteForwardPorts(); len(ports) > 0 {
		m.DeviceID = c.deviceID
		m.RemoteForwards = ports
	}
	return m
}

// handleRFwdStream はサーバからの -R 中継要求 1 本を処理する（先頭フレーム読み取り済み）。
func (c *Client) handleRFwdStream(st *quic.Stream, m *ctrlMsg) {
	reject := func(reason string) {
		_ = writeFrame(st, &ctrlMsg{Type: ctrlRFwdOpenErr, Msg: reason})
		st.CancelRead(fwdCodeProtocol)
		_ = st.Close()
	}
	target, ok := c.rfwdTargets[m.Port]
	if !ok {
		reject(fmt.Sprintf("no -R defined for port %d on this client", m.Port))
		return
	}
	conn, err := net.DialTimeout("tcp", target, forwardDialTimeout)
	if err != nil {
		c.logf("-R %d: dial %s failed: %v", m.Port, target, err)
		reject(err.Error())
		return
	}
	tconn := conn.(*net.TCPConn)
	if err := writeFrame(st, &ctrlMsg{Type: ctrlRFwdOpenOK}); err != nil {
		_ = tconn.Close()
		st.CancelRead(fwdCodeAbort)
		st.CancelWrite(fwdCodeAbort)
		return
	}
	c.rfwdActive.Add(1)
	defer c.rfwdActive.Add(-1)
	// バイト数はサーバ側（Manager の登録表）で計上するので、ここでは捨てる。
	var discardTo, discardFrom atomic.Uint64
	forwardPipe(st, tconn, &discardFrom, &discardTo)
}
