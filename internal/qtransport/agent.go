package qtransport

// agent.go: SSH agent forwarding（-A）の QUIC 側実装。
// 設計は docs/dev/agent-forwarding.md。forward.go（-L）と対称だが向きが逆:
// サーバがセッションの現在の agent provider クライアントへ新しい bidi ストリームを開き、
// 先頭に ctrlAgentOpen を送る。クライアントはローカルの $SSH_AUTH_SOCK に dial して
// ctrlAgentOpenOK / ctrlAgentOpenErr を返し、以降は生バイトの双方向中継（forwardPipe を共用）。
//
// provider 選出（「最後の Hello が勝つ」）は server.go の handleConn（Hello 受信時・
// 切断時）で行う。ここには OpenAgentStream（サーバ側の開始点）と、クライアント側の
// 受け口（acceptServerStreams・dial・中継）を置く。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

// agentDialTimeout はクライアント側のローカル $SSH_AUTH_SOCK dial のタイムアウト。
// ローカルソケットなので通常は瞬時に成否が決まる。
const agentDialTimeout = 3 * time.Second

// ---- サーバ側 ----

// SetAgentForwarding allows or denies SSH agent forwarding (-A). Default:
// allowed.
func (s *Server) SetAgentForwarding(enabled bool) {
	s.agentFwdDisabled.Store(!enabled)
}

// features は ctrlServerMeta で広告する機能ビットを返す。
func (s *Server) features() uint64 {
	f := FeatureLiveness | FeatureInputApplied
	if !s.fwdDisabled.Load() {
		f |= FeatureTCPForward
	}
	if s.remoteForwardEnabled() {
		f |= FeatureRemoteForward
	}
	if !s.agentFwdDisabled.Load() {
		f |= FeatureAgentForward
	}
	return f
}

// OpenAgentStream opens a relay channel to the session's current agent
// provider client (the reverse direction of -L's OpenForward). It returns an
// error when no provider is set (no -A client attached) or agent forwarding
// is disabled; the caller then closes the local socket connection. ctx
// applies only to the open handshake.
func (s *Server) OpenAgentStream(ctx context.Context, sessionID string) (ForwardConn, error) {
	if s.agentFwdDisabled.Load() {
		return nil, errors.New("agent forwarding disabled by server")
	}
	s.mu.Lock()
	provider, ok := s.agentProviders[sessionID]
	var sc *serverClient
	if ok {
		sc = s.clients[provider]
	}
	s.mu.Unlock()
	if !ok || sc == nil {
		return nil, errors.New("no agent forwarding provider attached")
	}

	// provider が見つかった時点で監査ログに残す（-L の dial 先ログと同じ方針。
	// 署名要求という機微な操作なので、既定で（debug フラグなしで）記録する。
	// 「provider 不在」は運用上ありふれた失敗モードなのでここではログしない。
	log.Printf("qtransport: agent forward: session %s -> provider client#%d", sessionID, provider.Num)

	st, err := openBidiStream(ctx, sc.conn, &ctrlMsg{Type: ctrlAgentOpen}, ctrlAgentOpenOK, ctrlAgentOpenErr)
	if err != nil {
		var rej *errRejected
		if errors.As(err, &rej) {
			log.Printf("qtransport: agent forward: session %s rejected by provider client#%d: %s", sessionID, provider.Num, rej.reason)
			return nil, fmt.Errorf("agent open rejected by client: %s", rej.reason)
		}
		return nil, fmt.Errorf("agent open: %w", err)
	}
	return &forwardStream{st: st}, nil
}

// ---- クライアント側 ----

// AgentForwardingSupported reports whether the server advertises -A support
// (known once serverMeta has been received; false until then).
func (c *Client) AgentForwardingSupported() bool {
	return c.serverFeatures.Load()&FeatureAgentForward != 0
}

// acceptServerStreams はサーバが開く control 以外の bidi ストリーム（-A・-R の中継要求）を受ける。
// forward.go の acceptForwardStreams と対称（サーバ→クライアント方向）。
func (c *Client) acceptServerStreams(conn *quic.Conn) {
	for {
		st, err := conn.AcceptStream(c.ctx)
		if err != nil {
			return
		}
		go c.handleServerStream(st)
	}
}

func (c *Client) handleServerStream(st *quic.Stream) {
	_ = st.SetReadDeadline(time.Now().Add(forwardOpenTimeout))
	m, err := readFrame(st)
	_ = st.SetReadDeadline(time.Time{})
	if err != nil {
		st.CancelRead(fwdCodeProtocol)
		st.CancelWrite(fwdCodeProtocol)
		return
	}
	switch m.Type {
	case ctrlAgentOpen:
		c.handleAgentStream(st)
	case ctrlRFwdOpen:
		c.handleRFwdStream(st, m)
	default:
		st.CancelRead(fwdCodeProtocol)
		st.CancelWrite(fwdCodeProtocol)
	}
}

// handleAgentStream はサーバからの agent 中継要求 1 本を処理する（先頭フレーム読み取り済み）。
func (c *Client) handleAgentStream(st *quic.Stream) {
	reject := func(reason string) {
		_ = writeFrame(st, &ctrlMsg{Type: ctrlAgentOpenErr, Msg: reason})
		st.CancelRead(fwdCodeProtocol)
		_ = st.Close()
	}

	sockPath := c.agentSockPath
	if sockPath == "" {
		reject("agent forwarding not enabled on this client")
		return
	}
	conn, err := net.DialTimeout("unix", sockPath, agentDialTimeout)
	if err != nil {
		reject(err.Error())
		return
	}
	uconn := conn.(*net.UnixConn)
	if err := writeFrame(st, &ctrlMsg{Type: ctrlAgentOpenOK}); err != nil {
		_ = uconn.Close()
		st.CancelRead(fwdCodeAbort)
		st.CancelWrite(fwdCodeAbort)
		return
	}

	// agent forwarding はバイト数を計上しない（-info への統計露出は未決事項。
	// docs/dev/agent-forwarding.md 参照）。forwardPipe の半クローズ規約だけ共用する。
	var discardTo, discardFrom atomic.Uint64
	forwardPipe(st, uconn, &discardTo, &discardFrom)
}
