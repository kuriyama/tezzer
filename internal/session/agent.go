package session

// agent.go: SSH agent forwarding（-A）のサーバ側（session 層）実装。
// 設計は docs/dev/agent-forwarding.md。ローカル UDS の accept ループと、
// transport.AgentForwarder 経由で agent provider クライアントへの中継を担う。
// QUIC 側の実装（provider 選出・ctrlAgentOpen ハンドシェイク）は internal/qtransport/agent.go。

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/transport"
)

// maxAgentStreams はセッションあたりの同時 agent 中継数の上限。
// ssh の agent multiplexing でも通常数本程度で足りる。
const maxAgentStreams = 8

// agentOpenTimeout はサーバ側から provider への中継路確立（OpenAgentStream）のタイムアウト。
const agentOpenTimeout = 15 * time.Second

// newAgentListener は agent forwarding 用の per-session UDS を bind する。
func newAgentListener(sessionID string) (*net.UnixListener, string, error) {
	path, err := netx.GetAgentSocketPath(sessionID)
	if err != nil {
		return nil, "", err
	}
	_ = os.Remove(path) // 残骸（異常終了等）を掃除してから bind
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, "", err
	}
	return ln, path, nil
}

// acceptAgentConns はローカル UDS への接続を受け、QUIC 経由で agent provider クライアントへ中継する。
func (s *Session) acceptAgentConns() {
	for {
		uconn, err := s.agentListener.AcceptUnix()
		if err != nil {
			return // listener close（Session.Close）で終了
		}
		go s.handleAgentConn(uconn)
	}
}

// handleAgentConn は 1 本の UDS 接続を、現在の agent provider クライアントへ中継する。
// provider 不在（未接続・切断済み）の場合は ssh の通常の失敗モードと同様、即座に閉じる。
func (s *Session) handleAgentConn(uconn *net.UnixConn) {
	defer uconn.Close()

	if n := s.agentActive.Add(1); n > maxAgentStreams {
		s.agentActive.Add(-1)
		return
	}
	defer s.agentActive.Add(-1)

	af, ok := s.st.(transport.AgentForwarder)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentOpenTimeout)
	defer cancel()
	fc, err := af.OpenAgentStream(ctx, s.ID)
	if err != nil {
		if s.debug {
			log.Printf("session %s: agent forward: %v", s.ID, err)
		}
		return
	}
	defer fc.Close()
	pipeAgentConn(uconn, fc)
}

// pipeAgentConn は UDS 接続と QUIC 中継路を双方向に中継する。
// qtransport.forwardPipe と同じ半クローズ規約（正常 EOF は CloseWrite、エラーは Close）。
func pipeAgentConn(uconn *net.UnixConn, fc transport.ForwardConn) {
	done := make(chan struct{}, 2)
	go func() {
		if _, err := io.Copy(fc, uconn); err == nil {
			_ = fc.CloseWrite()
		} else {
			_ = fc.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		if _, err := io.Copy(uconn, fc); err == nil {
			_ = uconn.CloseWrite()
		} else {
			_ = uconn.Close()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}
