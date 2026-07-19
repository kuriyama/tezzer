package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuriyama/tezzer/internal/qtransport"
	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/kuriyama/tezzer/internal/version"
)

// Manager manages multiple sessions and an optional shared QUIC transport.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session

	// サーバーインスタンスID（サーバー再起動検知用、起動時にランダム生成）
	serverInstanceID [8]byte

	// 共有 QUIC transport（固定ポート運用＝1ポートで複数セッション相乗り時）。
	// QUIC は connection ID で多接続を多重化するので、1 ソケットを全セッションで共有できる。
	// fixedPort 未指定時は nil（各セッションが per-session で transport を持つ）。
	sharedTransport transport.ServerTransport
	sharedCancel    context.CancelFunc
	sharedPort      int
	sharedKey       []byte

	// TCP ポートフォワードの禁止フラグ（零値 = 許可。tezzerd --no-tcp-forwarding で true）
	tcpFwdDisabled atomic.Bool
	// agent forwarding（-A）の禁止フラグ（零値 = 許可。tezzerd --no-agent-forwarding で true）
	agentFwdDisabled atomic.Bool

	// アクティブなセッション数の上限（0 = 無制限。tezzerd --max-sessions で設定。mu 保護）
	maxSessions int

	// シャットダウン用
	done chan struct{}
}

// SetTCPForwarding allows or denies TCP forwarding for all sessions.
// Intended to be called at startup, before any session is created.
func (m *Manager) SetTCPForwarding(enabled bool) {
	m.tcpFwdDisabled.Store(!enabled)
}

// tcpForwardingEnabled は転送許可状態を返す（零値 = 許可）。
func (m *Manager) tcpForwardingEnabled() bool {
	return !m.tcpFwdDisabled.Load()
}

// SetAgentForwarding allows or denies SSH agent forwarding (-A) for all
// sessions. Intended to be called at startup, before any session is created.
func (m *Manager) SetAgentForwarding(enabled bool) {
	m.agentFwdDisabled.Store(!enabled)
}

// AgentForwardingEnabled reports whether agent forwarding is allowed (zero
// value = allowed). Used at session creation to decide whether to create the
// agent Unix socket (defense in depth in NewSession).
func (m *Manager) AgentForwardingEnabled() bool {
	return !m.agentFwdDisabled.Load()
}

// applyTransportPolicy は生成した transport にサーバポリシー（転送許可）を適用する。
func (m *Manager) applyTransportPolicy(st transport.ServerTransport) {
	if p, ok := st.(transport.ForwardingPolicy); ok {
		p.SetTCPForwarding(m.tcpForwardingEnabled())
		p.SetAgentForwarding(m.AgentForwardingEnabled())
	}
}

// InitSharedTransport starts one shared QUIC transport for fixed-port
// deployments. It does nothing when fixedPort <= 0 (each session then owns a
// per-session transport). In shared mode one socket multiplexes every
// session, so several sessions can ride a single port-forwarded fixed port.
func (m *Manager) InitSharedTransport(fixedPort int, ipv4Only bool) error {
	if fixedPort <= 0 {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("failed to generate shared key: %w", err)
	}
	st, err := qtransport.NewServer(key, fmt.Sprintf(":%d", fixedPort))
	if err != nil {
		return fmt.Errorf("failed to create shared QUIC transport: %w", err)
	}
	a, ok := st.(transport.Addresser)
	if !ok {
		_ = st.Close()
		return fmt.Errorf("transport does not expose listen address")
	}
	addr, ok := a.Addr().(*net.UDPAddr)
	if !ok {
		_ = st.Close()
		return fmt.Errorf("failed to get shared QUIC listen address")
	}
	return m.adoptSharedTransport(st, addr.Port, key)
}

// adoptSharedTransport は共有 QUIC トランスポートの配線・起動を行う
// （InitSharedTransport と無停止再起動の復元経路で共通）。失敗時は st を閉じる。
func (m *Manager) adoptSharedTransport(st transport.ServerTransport, port int, key []byte) error {
	st.OnResyncNeeded(m.sharedResync)
	st.OnClientConnect(m.sharedOnConnect)
	st.SetServerMeta(version.GetVersion(), version.GetBuildTime(), m.serverInstanceID[:])
	m.applyTransportPolicy(st)

	ctx, cancel := context.WithCancel(context.Background())
	if err := st.Start(ctx); err != nil {
		cancel()
		_ = st.Close()
		return fmt.Errorf("failed to start shared QUIC transport: %w", err)
	}

	m.sharedTransport = st
	m.sharedCancel = cancel
	m.sharedPort = port
	m.sharedKey = key

	go m.routeSharedInput()
	go m.routeSharedResize()
	log.Printf("shared QUIC transport enabled on port %d", m.sharedPort)
	return nil
}

// IsSharedTransportEnabled reports whether shared-transport mode is active.
func (m *Manager) IsSharedTransportEnabled() bool { return m.sharedTransport != nil }

// GetSharedPort / GetSharedKey expose the shared transport's port and key,
// announced to clients during bootstrap.
func (m *Manager) GetSharedPort() int   { return m.sharedPort }
func (m *Manager) GetSharedKey() []byte { return m.sharedKey }

// sessionByID は SessionID から該当セッションを引く。
func (m *Manager) sessionByID(sessionID string) *Session {
	m.mu.RLock()
	sess := m.sessions[sessionID]
	m.mu.RUnlock()
	return sess
}

// sharedResync は共有 transport の OnResyncNeeded を Client.Session で振り分ける。
func (m *Manager) sharedResync(client transport.ClientID, fromOffset uint64) ([]transport.OutputChunk, error) {
	sess := m.sessionByID(client.Session)
	if sess == nil {
		return nil, nil
	}
	return sess.outputFromOffset(client, fromOffset)
}

// sharedOnConnect は接続クライアントのセッションが存在するか検証し、消えていれば
// （サーバ再起動後の reconnect・kill 済み等）QUIC 経路で sessionGone を通知する。
func (m *Manager) sharedOnConnect(client transport.ClientID) {
	sess := m.sessionByID(client.Session)
	if sess == nil {
		if m.sharedTransport != nil {
			_ = m.sharedTransport.SendSessionGone(client, "NO_SUCH_SESSION: session not found", -1)
		}
		return
	}
	// Gate 2: QUIC が実際に接続した時点でセッションに通知（DA 漏洩防止）
	sess.OnQUICClientConnected()
}

// routeSharedInput は共有 transport の入力を Client.Session で PTY へ振り分ける。
// 識別子は Hello に載った (SessionID, Num) の複合 ClientID なので、別セッションの同じ Num と
// 衝突しない。
func (m *Manager) routeSharedInput() {
	for {
		select {
		case <-m.done:
			return
		case in, ok := <-m.sharedTransport.Input():
			if !ok {
				return
			}
			if sess := m.sessionByID(in.Client.Session); sess != nil {
				if err := sess.WriteInput(in.Data); err != nil && !sess.IsPTYClosed() {
					log.Printf("shared QUIC: write input to session failed: %v", err)
				}
			}
		}
	}
}

// routeSharedResize は共有 transport のリサイズを Client.Session で振り分ける。
func (m *Manager) routeSharedResize() {
	for {
		select {
		case <-m.done:
			return
		case rz, ok := <-m.sharedTransport.Resize():
			if !ok {
				return
			}
			if sess := m.sessionByID(rz.Client.Session); sess != nil {
				_ = sess.Resize(rz.Rows, rz.Cols)
			}
		}
	}
}

// NewManager creates a new session manager
func NewManager() *Manager {
	m := &Manager{
		sessions: make(map[string]*Session),
		done:     make(chan struct{}),
	}
	// サーバーインスタンスIDを生成（サーバー再起動検知用）
	if _, err := rand.Read(m.serverInstanceID[:]); err != nil {
		// フォールバック: 時刻ベースのID
		binary.BigEndian.PutUint64(m.serverInstanceID[:], uint64(time.Now().UnixNano()))
	}
	go m.runSessionCleanup()
	return m
}

// runSessionCleanup は PTY 終了済みで接続クライアントがいないセッションを定期削除する
func (m *Manager) runSessionCleanup() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.cleanupClosedSessions()
			m.evictStaleOutputBuffers()
		}
	}
}

// evictStaleOutputBuffers は全セッションの保持期限切れ出力チャンクを回収する。
// 出力が止まったセッションは追記時 evict の契機がないため、定期実行で補う。
func (m *Manager) evictStaleOutputBuffers() {
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()
	for _, s := range sessions {
		s.EvictStaleOutput()
	}
}

// cleanupClosedSessions は PTY 終了済み・クライアント数 0・終了から30秒以上のセッションを削除する
func (m *Manager) cleanupClosedSessions() {
	const graceperiod = 30 * time.Second

	m.mu.RLock()
	var toDelete []string
	for id, sess := range m.sessions {
		if sess.IsPTYClosed() &&
			sess.GetClientCount() == 0 &&
			time.Since(sess.GetPTYClosedAt()) > graceperiod {
			toDelete = append(toDelete, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range toDelete {
		if err := m.DeleteSession(id); err == nil {
			log.Printf("session %s: auto-deleted (PTY terminated, no clients)", id)
		}
	}
}

// GetServerInstanceID returns the server instance ID (restart detection).
func (m *Manager) GetServerInstanceID() [8]byte {
	return m.serverInstanceID
}

// ErrDuplicateName indicates an active session with the same name already
// exists.
var ErrDuplicateName = errors.New("session name already in use")

// ErrTooManySessions indicates the active session count has reached the
// --max-sessions limit.
var ErrTooManySessions = errors.New("session limit reached")

// SetMaxSessions sets the limit on active sessions (0 = unlimited).
// Intended to be called once at server startup.
func (m *Manager) SetMaxSessions(n int) {
	m.mu.Lock()
	m.maxSessions = n
	m.mu.Unlock()
}

// activeSessionCountLocked は PTY 終了済みでないセッション数を返す。
// （終了済みは cleanup 待ちの残骸なので上限に数えない）m.mu を保持して呼ぶこと。
func (m *Manager) activeSessionCountLocked() int {
	n := 0
	for _, s := range m.sessions {
		if !s.IsPTYClosed() {
			n++
		}
	}
	return n
}

// FindByName returns the active (PTY not yet ended) session with the given
// name. Sessions whose PTY has ended are leftovers awaiting cleanup and are
// treated as no longer holding their name.
func (m *Manager) FindByName(name string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.Name == name && !s.IsPTYClosed() {
			return s
		}
	}
	return nil
}

// CreateSession creates a new session. An empty name creates an unnamed
// session; a non-empty name is guaranteed unique among active sessions
// (ErrDuplicateName otherwise). agentForward requests SSH agent forwarding
// (-A); it only has meaning at creation time (docs/dev/agent-forwarding.md).
func (m *Manager) CreateSession(name, cmd string, args []string, env map[string]string, cwd string, rows, cols int, ipv4Only bool, fixedUDPPort int, agentForward bool) (*Session, error) {
	// PTY/transport を起動する前の事前チェック（無駄な起動を避ける）
	if name != "" && m.FindByName(name) != nil {
		return nil, fmt.Errorf("%w: %s", ErrDuplicateName, name)
	}
	m.mu.RLock()
	limit := m.maxSessions
	overLimit := limit > 0 && m.activeSessionCountLocked() >= limit
	m.mu.RUnlock()
	if overLimit {
		return nil, fmt.Errorf("%w (max %d)", ErrTooManySessions, limit)
	}

	id := generateSessionID()

	// full-QUIC 化に伴い共有モードは当面未対応。各セッションが per-session で
	// qtransport を起動する（NewSession 内）。
	session, err := NewSession(id, cmd, args, env, cwd, rows, cols, ipv4Only, fixedUDPPort, agentForward, m)
	if err != nil {
		return nil, err
	}
	session.Name = name

	m.mu.Lock()
	// 登録直前に再チェック（同名・上限の並行 CREATE との競合をここで一本化）
	if name != "" {
		for _, s := range m.sessions {
			if s.Name == name && !s.IsPTYClosed() {
				m.mu.Unlock()
				session.Close()
				return nil, fmt.Errorf("%w: %s", ErrDuplicateName, name)
			}
		}
	}
	if m.maxSessions > 0 && m.activeSessionCountLocked() >= m.maxSessions {
		limit := m.maxSessions
		m.mu.Unlock()
		session.Close()
		return nil, fmt.Errorf("%w (max %d)", ErrTooManySessions, limit)
	}
	m.sessions[id] = session
	m.mu.Unlock()

	return session, nil
}

// GetSession retrieves a session by ID
func (m *Manager) GetSession(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	session, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}

	return session, nil
}

// DeleteSession deletes a session
func (m *Manager) DeleteSession(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	session.Close()
	delete(m.sessions, id)

	return nil
}

// ListSessions returns all session IDs
func (m *Manager) ListSessions() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}

	return ids
}

// GetAllSessions returns all sessions sorted by CreatedAt (oldest first)
func (m *Manager) GetAllSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}

	// CreatedAt の古い順にソート
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
	})

	return sessions
}

// SetDebugForAllSessions toggles debug output for every session at runtime.
func (m *Manager) SetDebugForAllSessions(enabled bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, session := range m.sessions {
		session.SetDebug(enabled)
	}
}

// Close cleans up the manager's resources.
func (m *Manager) Close() {
	close(m.done)
	if m.sharedTransport != nil {
		if m.sharedCancel != nil {
			m.sharedCancel()
		}
		_ = m.sharedTransport.Close()
	}
}

// generateSessionID generates a random session ID using base64 URL encoding without padding
func generateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand は実質失敗しないが、握り潰さない（衝突しない ID 生成の前提が崩れるため）
		panic("generateSessionID: " + err.Error())
	}
	// base64 URLエンコード（パディングなし、URLセーフ）
	// 16バイト → 22文字（base32の26文字より短い）
	return base64.RawURLEncoding.EncodeToString(b)
}
