package main

// connect.go: QUIC トランスポートの確立まわり。
// SESSION_CREATED の接続情報（QUICInfo）から候補アドレスを生成し（STUN 併用）、
// 全候補へ並行 Dial して最初に確立した接続を採用する。確立後のコールバック配線・
// ローミング候補・候補リフレッシャーの登録もここ。初回確立失敗時のバックグラウンド
// 再試行（retryQUICTransport）を含む。

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/qtransport"
	"github.com/kuriyama/tezzer/internal/stun"
	"github.com/kuriyama/tezzer/internal/transport"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// QUICInfo は QUIC 接続情報（ポート・共有鍵・候補アドレス）を保持
type QUICInfo struct {
	Port       int
	Key        []byte
	STUNAddrs  []string // サーバーのSTUN経由アドレス候補（NAT越え用、family別）
	LocalAddr  string   // サーバーのローカルアドレス（LAN内接続用）
	STUNServer string   // サーバが使った STUN サーバー（クライアントも同じものを使う。空 = 既定）
}

// quicInfoFromSessionCreated は SESSION_CREATED 応答（create/attach 共通）から
// QUIC 接続情報を取り出す。QUIC 無効なら nil。
func quicInfoFromSessionCreated(m *proto.SessionCreatedMsg) *QUICInfo {
	if !m.UDPEnabled {
		return nil
	}
	return &QUICInfo{
		Port:       m.UDPPort,
		Key:        m.UDPKey,
		STUNAddrs:  m.STUNAddrs,
		LocalAddr:  m.LocalAddr,
		STUNServer: m.STUNServer,
	}
}

// buildQUICCandidateAddrs はサーバー情報とクライアント STUN アドレス候補から
// 接続候補リストを生成する純粋関数。startQUICTransport と候補リフレッシャーの両方で使用。
func buildQUICCandidateAddrs(quicInfo *QUICInfo, clientSTUNAddrs []string) []string {
	var candidates []string

	// クライアント・サーバー双方に STUN アドレスがあり、どの family の組でも
	// IP が一致しなければリモート接続とみなす（同一ホストなら少なくとも1つの
	// family で一致するはず）。
	isRemoteConnection := false
	if len(clientSTUNAddrs) > 0 && len(quicInfo.STUNAddrs) > 0 {
		isRemoteConnection = true
		for _, ca := range clientSTUNAddrs {
			clientIP, _, _ := net.SplitHostPort(ca)
			if clientIP == "" {
				continue
			}
			for _, sa := range quicInfo.STUNAddrs {
				serverIP, _, _ := net.SplitHostPort(sa)
				if serverIP != "" && clientIP == serverIP {
					isRemoteConnection = false
				}
			}
		}
	}

	if !isRemoteConnection && serverIsOnSameHost(quicInfo.LocalAddr) {
		candidates = append(candidates, fmt.Sprintf("127.0.0.1:%d", quicInfo.Port))
	}
	if !isRemoteConnection && quicInfo.LocalAddr != "" && !strings.HasPrefix(quicInfo.LocalAddr, "127.") {
		candidates = append(candidates, quicInfo.LocalAddr)
	}
	for _, addr := range quicInfo.STUNAddrs {
		if addr != "" {
			candidates = append(candidates, addr)
		}
	}
	// 候補ゼロ（完全オフライン環境などで LocalAddr も STUN も取れない）の場合、
	// 最後の頼みとして loopback を試す。ローカル利用(UDS 直結)ならこれで繋がる。
	// サーバが実際には別ホストでも mTLS（K の pinning）検証で安全に失敗するだけ。
	if len(candidates) == 0 && !isRemoteConnection {
		candidates = append(candidates, fmt.Sprintf("127.0.0.1:%d", quicInfo.Port))
	}
	return candidates
}

// startQUICTransport は QUIC トランスポートを確立し、コールバック・候補リフレッシャーを配線する。
func (c *Client) startQUICTransport(udsAddr string, quicInfo *QUICInfo) error {
	// 接続試行する候補アドレスのリストを決定（優先順位順）
	var candidateAddrs []string

	// クライアント識別子を生成（16-bit ランダム値）。0 は「QUIC 未使用」の番兵として
	// 各所（UDS 配信抑制・-info 表示）が解釈するため避ける。0 のままだと QUIC 接続済み
	// でも UDS 併送が止まらず、WAN 帯域が二重になる。
	var clientID uint16
	for clientID == 0 {
		var clientIDBytes [2]byte
		if _, err := rand.Read(clientIDBytes[:]); err != nil {
			return fmt.Errorf("failed to generate client ID: %w", err)
		}
		clientID = binary.LittleEndian.Uint16(clientIDBytes[:])
	}

	// クライアント側もSTUNで自分の公開アドレス候補を取得（リモート接続判定 =
	// 候補アドレス生成にクライアントローカルで使うだけで、サーバへは送らない）
	var clientSTUNAddrs []string
	if len(quicInfo.STUNAddrs) > 0 {
		clientSTUNAddrs = c.getClientSTUNAddrs(quicInfo.STUNServer)
		if len(clientSTUNAddrs) > 0 {
			c.setStatusMessage(fmt.Sprintf("client STUN address: %s", strings.Join(clientSTUNAddrs, ", ")))
		} else {
			c.setStatusMessage("warning: failed to get client STUN address")
		}
	}

	// clientID をサーバーに通知（UDS経由）。出力ファンアウト対象への登録に必要
	if err := c.sendQUICClientInfo(clientID); err != nil {
		c.setStatusMessage(fmt.Sprintf("warning: failed to send client ID info: %v", err))
	}

	candidateAddrs = buildQUICCandidateAddrs(quicInfo, clientSTUNAddrs)

	// 候補アドレスを常時ログ＆通知（debug 不要で「どのアドレス/ポートを叩いたか」を診断できる）
	if len(candidateAddrs) > 0 {
		c.setStatusMessage(fmt.Sprintf("QUIC candidates: %s", strings.Join(candidateAddrs, ", ")))
		if c.fileLog != nil {
			c.fileLog.Printf("QUIC candidates: %s", strings.Join(candidateAddrs, ", "))
		}
	}

	// 候補アドレスを表示
	if c.isDebugEnabled() {
		for i, addr := range candidateAddrs {
			log.Printf("QUIC candidate %d: %s", i+1, addr)
		}
	}

	// QUIC トランスポートを構築（認証は quicInfo.Key）。候補は SetRoamingCandidates で
	// 渡し、全候補への並行 Dial は transport 内部（Start = reconnect と同じ dialParallel）
	// が行う。最初に確立した候補が採用される（localhost / LAN / グローバル STUN の選択）。
	if len(candidateAddrs) == 0 {
		return fmt.Errorf("no QUIC candidate addresses")
	}

	ct, err := qtransport.NewClient(quicInfo.Key, candidateAddrs[0], clientID, c.sessionID)
	if err != nil {
		return fmt.Errorf("failed to create QUIC transport: %w", err)
	}
	if c.agentSockPath != "" {
		if as, ok := ct.(transport.AgentForwardClient); ok {
			as.SetAgentSockPath(c.agentSockPath)
		}
	}
	// UDS 経由で既に描画済みの分は QUIC の再同期対象から外す
	// （attach 直後のバックログ二重転送の防止。Hello の LastOffset に反映される）。
	if seed := c.renderedSeq.Load(); seed > 0 {
		if sr, ok := ct.(transport.ResyncSeeder); ok {
			sr.SeedResyncOffset(seed)
		}
	}

	// コールバックは Start 前に登録する（初回接続の state 遷移「connected via <addr>」や
	// 診断ログを取りこぼさないため）。
	ct.OnStateChange(func(oldState, newState transport.ConnectionState, message string) {
		c.setStatusMessage(fmt.Sprintf("[QUIC] %s", message))
		c.addLogMessage(fmt.Sprintf("[QUIC] state: %s", message))
		if newState == transport.StateConnected && oldState == transport.StateRecovering {
			c.requestServerRedraw()
		}
	})
	ct.OnStatusMessage(func(message string) { c.setStatusMessage(message) })
	ct.OnLogMessage(func(message string) { c.addLogMessage(message) })
	ct.OnServerMeta(func(buildID, buildTime string, instanceID []byte) {
		c.metaMu.Lock()
		c.serverMeta = &proto.ServerMetaMsg{
			Type:             "SERVER_META",
			ServerBuildID:    buildID,
			ServerBuildTime:  buildTime,
			ServerInstanceID: instanceID,
		}
		c.metaMu.Unlock()
	})
	ct.OnSessionNotFound(func(reason string, exitCode int) {
		if c.killing.Load() {
			// killSession() の結果として届いた sessionGone は正常終了扱い
			select {
			case c.errCh <- errDetached:
			default:
			}
			return
		}
		if strings.HasPrefix(reason, "SESSION_CLOSED:") {
			// PTY プロセス終了による通知。終了コードを記録して伝搬する。
			c.noteSessionExitCode(exitCode)
			// UDS 経由で先に表示済みでなければここで出す
			// （QUIC-only クライアント、または QUIC が UDS より先に届いたケース）。
			if !c.sessionClosedNotified.Load() {
				msg := strings.TrimPrefix(reason, "SESSION_CLOSED: ")
				fmt.Fprintf(os.Stderr, "\r\n[Tezzer] Session closed: %s\r\n", msg)
			}
			select {
			case c.errCh <- errDetached:
			default:
			}
			return
		}
		select {
		case c.errCh <- fmt.Errorf("%s", reason):
		default:
		}
	})

	// 全候補失敗時の候補リフレッシャーを登録する。
	// reconnect がすべて失敗した場合のみ呼ばれる（通常 reconnect にはコスト増なし）。
	quicInfoCopy := quicInfo // クロージャにコピーを渡す
	ct.SetCandidatesRefresher(func() []string {
		var clientSTUN []string
		if len(quicInfoCopy.STUNAddrs) > 0 {
			clientSTUN = c.getClientSTUNAddrs(quicInfoCopy.STUNServer)
		}
		fresh := buildQUICCandidateAddrs(quicInfoCopy, clientSTUN)
		if len(fresh) > 0 {
			c.addLogMessage(fmt.Sprintf("QUIC: refreshed candidates: %s", strings.Join(fresh, ", ")))
		}
		return fresh
	})

	// 高遅延リンク（RTT 数秒）でも成功できるよう全体で 10s（並行 Dial なので直列より速い）。
	ct.SetRoamingCandidates(candidateAddrs)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	if err := ct.Start(dialCtx); err != nil {
		_ = ct.Close()
		c.setStatusMessage("QUIC: " + err.Error())
		if c.fileLog != nil {
			c.fileLog.Printf("QUIC: %v", err)
		}
		return fmt.Errorf("failed to connect: %w", err)
	}

	c.setTransport(ct)
	return nil
}

// retryQUICTransport は QUIC 初回確立に失敗した場合に、UDS が生きている間
// バックグラウンドで再試行する。成功したら出力ポンプを起動して昇格する。
func (c *Client) retryQUICTransport(udsAddr string, quicInfo *QUICInfo) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if c.transport() != nil {
				return // 既に確立済み（別経路で昇格した等）
			}
			if err := c.startQUICTransport(udsAddr, quicInfo); err != nil {
				if c.isDebugEnabled() {
					log.Printf("QUIC: background retry failed: %v", err)
				}
				continue
			}
			c.setStatusMessage("[Tezzer] QUIC connection established (background retry)")
			go c.handleQUICOutput()
			return
		}
	}
}

// defaultSTUNServer は旧サーバ（SESSION_CREATED に stun_server が載らない）に
// 接続した場合のフォールバック。
const defaultSTUNServer = "stun.l.google.com:19302"

// getClientSTUNAddrs はクライアント側のSTUNアドレス候補を family ごとに取得する
// （リモート接続判定 = 接続候補生成のため。ipv4Only なら v4 のみ問い合わせる）。
// stunServer はサーバが SESSION_CREATED で伝えた --stun-server の値（空なら既定）。
func (c *Client) getClientSTUNAddrs(stunServer string) []string {
	if stunServer == "" {
		stunServer = defaultSTUNServer
	}
	var addrs []string
	if addr, err := c.getClientSTUNAddr("udp4", stunServer); err == nil {
		addrs = append(addrs, addr.String())
	}
	if !c.ipv4Only {
		if addr, err := c.getClientSTUNAddr("udp6", stunServer); err == nil {
			addrs = append(addrs, addr.String())
		}
	}
	return addrs
}

// getClientSTUNAddr はクライアント側のSTUNアドレスを指定 family（"udp4"/"udp6"）で取得する。
// 旧実装はここで NAT タイプ判別（DetectNATType）も行っていたが、クエリごとに別ソケットを
// 使うため port 比較が成立せずほぼ常に Symmetric と誤報告しており、結果も表示にしか
// 使っていなかったため撤去した（正しい同一ソケット判別は stun.Probe / tezzerd -check-nat）。
func (c *Client) getClientSTUNAddr(network, stunServer string) (*net.UDPAddr, error) {
	stunClient := stun.NewClient(stunServer)
	stunClient.Network = network
	return stunClient.GetMappedAddr()
}

// serverIsOnSameHost はサーバーの LocalAddr がクライアント自身のインターフェース IP と
// 一致するかを返す（同一ホスト判定）。一致しない・不明な場合は false。
func serverIsOnSameHost(serverLocalAddr string) bool {
	if serverLocalAddr == "" {
		return false
	}
	serverIP, _, err := net.SplitHostPort(serverLocalAddr)
	if err != nil || serverIP == "" {
		return false
	}
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range ifaces {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.String() == serverIP {
			return true
		}
	}
	return false
}

// sendQUICClientInfo はクライアントの clientID をサーバーに UDS 経由で通知する
// （出力ファンアウト対象への登録。wire のメッセージ型名は互換のため UDP_CLIENT_INFO のまま）。
func (c *Client) sendQUICClientInfo(clientID uint16) error {
	msg := proto.UDPClientInfoMsg{
		Type:      "UDP_CLIENT_INFO",
		SessionID: c.sessionID,
		ClientID:  clientID,
	}
	data, err := proto.Encode(msg)
	if err != nil {
		return fmt.Errorf("failed to encode client ID info: %w", err)
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return netx.WriteFrame(c.conn, data)
}

// requestServerMeta はサーバーのメタ情報を要求する（送信のみ）。
// 応答（SERVER_META）は handleServer が受信して c.serverMeta に格納する。
// ここで応答を読んではいけない: 同じ UDS conn を handleServer が常時 ReadFrame
// しており、2 goroutine から読むとフレーム境界が壊れる。
func (c *Client) requestServerMeta() error {
	msg := proto.GetServerMetaMsg{
		Type:      "GET_SERVER_META",
		SessionID: c.sessionID,
	}
	data, err := proto.Encode(msg)
	if err != nil {
		return fmt.Errorf("failed to encode get server meta: %w", err)
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return netx.WriteFrame(c.conn, data)
}
