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
	udpPort         int
	udpKey          []byte

	// TCP ポートフォワードの禁止フラグ（零値 = 許可。tezzerd --no-tcp-forwarding で true）
	tcpFwdDisabled atomic.Bool
	// agent forwarding（-A）の禁止フラグ（零値 = 許可。tezzerd --no-agent-forwarding で true）
	agentFwdDisabled atomic.Bool

	// アクティブなセッション数の上限（0 = 無制限。tezzerd --max-sessions で設定。mu 保護）
	maxSessions int

	// シャットダウン用
	done chan struct{}
}

// SetTCPForwarding は全セッションの TCP 転送の許可/禁止を設定する。
// セッション作成前（起動時）に呼ぶ想定。
func (m *Manager) SetTCPForwarding(enabled bool) {
	m.tcpFwdDisabled.Store(!enabled)
}

// tcpForwardingEnabled は転送許可状態を返す（零値 = 許可）。
func (m *Manager) tcpForwardingEnabled() bool {
	return !m.tcpFwdDisabled.Load()
}

// SetAgentForwarding は全セッションの agent forwarding（-A）の許可/禁止を設定する。
// セッション作成前（起動時）に呼ぶ想定。
func (m *Manager) SetAgentForwarding(enabled bool) {
	m.agentFwdDisabled.Store(!enabled)
}

// AgentForwardingEnabled は agent forwarding の許可状態を返す（零値 = 許可）。
// セッション作成時、UDS リスナーを作るかどうかの判断に使う（NewSession の二重防御）。
func (m *Manager) AgentForwardingEnabled() bool {
	return !m.agentFwdDisabled.Load()
}

// applyTransportPolicy は生成した transport にサーバポリシー（転送許可）を適用する。
func (m *Manager) applyTransportPolicy(st transport.ServerTransport) {
	if f, ok := st.(interface{ SetTCPForwarding(bool) }); ok {
		f.SetTCPForwarding(m.tcpForwardingEnabled())
	}
	if f, ok := st.(interface{ SetAgentForwarding(bool) }); ok {
		f.SetAgentForwarding(m.AgentForwardingEnabled())
	}
}

// InitSharedUDP は固定ポート運用時に共有 QUIC transport を1つ起動する。
// fixedPort <= 0 の場合は何もしない（各セッションが per-session で transport を持つ）。
// 共有時は 1 ソケットを全セッションで多重化し、固定ポートを port-forward した運用で
// 複数セッションを相乗りできる。
func (m *Manager) InitSharedUDP(fixedPort int, ipv4Only bool) error {
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
	addr, ok := st.(interface{ Addr() net.Addr }).Addr().(*net.UDPAddr)
	if !ok {
		_ = st.Close()
		return fmt.Errorf("failed to get shared QUIC listen address")
	}
	return m.adoptSharedTransport(st, addr.Port, key)
}

// adoptSharedTransport は共有 QUIC トランスポートの配線・起動を行う
// （InitSharedUDP と無停止再起動の復元経路で共通）。失敗時は st を閉じる。
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
	m.udpPort = port
	m.udpKey = key

	go m.routeSharedInput()
	go m.routeSharedResize()
	log.Printf("shared QUIC transport enabled on port %d", m.udpPort)
	return nil
}

// IsSharedUDPEnabled は共有 transport モードかを返す。
func (m *Manager) IsSharedUDPEnabled() bool { return m.sharedTransport != nil }

// GetSharedUDPPort / GetSharedUDPKey は bootstrap で client へ伝える用。
func (m *Manager) GetSharedUDPPort() int   { return m.udpPort }
func (m *Manager) GetSharedUDPKey() []byte { return m.udpKey }

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

// GetServerInstanceID はサーバーインスタンスIDを返す（サーバー再起動検知用）
func (m *Manager) GetServerInstanceID() [8]byte {
	return m.serverInstanceID
}

// ErrDuplicateName は同名のアクティブなセッションが既に存在することを示す。
var ErrDuplicateName = errors.New("session name already in use")

// ErrTooManySessions はアクティブなセッション数が上限（--max-sessions）に達していることを示す。
var ErrTooManySessions = errors.New("session limit reached")

// SetMaxSessions はアクティブなセッション数の上限を設定する（0 = 無制限）。
// サーバ起動時に一度だけ呼ぶ想定。
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

// FindByName は名前が一致するアクティブな（PTY 終了済みでない）セッションを返す。
// PTY 終了済みセッションは cleanup 待ちの残骸なので名前を保持しない扱いにする。
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

// CreateSession creates a new session.
// name は空なら無名セッション。非空なら アクティブなセッション間で一意 を保証する
// （同名があれば ErrDuplicateName）。
// agentForward は -A（SSH agent forwarding）の要求。作成時にのみ意味を持つ
// （docs/dev/agent-forwarding.md）。
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

// SetDebugForAllSessions は全セッションのデバッグ出力の有効/無効を動的に切り替える
func (m *Manager) SetDebugForAllSessions(enabled bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, session := range m.sessions {
		session.SetDebug(enabled)
	}
}

// Close はマネージャのリソースをクリーンアップする。
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
