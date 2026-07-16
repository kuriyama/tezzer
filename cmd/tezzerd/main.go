package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/session"
	"github.com/kuriyama/tezzer/internal/stun"
	"github.com/kuriyama/tezzer/internal/version"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// デバッグ出力のグローバル制御（atomic操作用、0=OFF, 1=ON）
var debugEnabled int32

// isDebugEnabled はデバッグ出力が有効かどうかを返す
func isDebugEnabled() bool {
	return atomic.LoadInt32(&debugEnabled) == 1
}

func main() {
	listenUnix := flag.String("listen-unix", "", "Unix Domain Socket path (default: $XDG_RUNTIME_DIR/tezzer.sock or ~/.tezzer/tezzer.sock)")
	stunServer := flag.String("stun-server", "stun.l.google.com:19302", "STUN server address for NAT traversal")
	ipv4Only := flag.Bool("ipv4-only", false, "use IPv4 only for STUN and UDP connections")
	udpPort := flag.Int("udp-port", 0, "fixed UDP port for sessions (default: auto-assign)")
	noTCPForwarding := flag.Bool("no-tcp-forwarding", false, "disable TCP port forwarding (-L) for all sessions")
	noAgentForwarding := flag.Bool("no-agent-forwarding", false, "disable SSH agent forwarding (-A) for all sessions")
	maxSessions := flag.Int("max-sessions", 0, "maximum number of active sessions (0 = unlimited)")
	flag.Parse()

	// デバッグフラグの初期値（環境変数から）
	debugInit := os.Getenv("TEZZER_DEBUG") != ""
	if debugInit {
		atomic.StoreInt32(&debugEnabled, 1)
	}
	session.SetDebugEnabled(debugInit)

	mgr := session.NewManager()
	if *noTCPForwarding {
		mgr.SetTCPForwarding(false)
		log.Printf("TCP port forwarding disabled (--no-tcp-forwarding)")
	}
	if *noAgentForwarding {
		mgr.SetAgentForwarding(false)
		log.Printf("SSH agent forwarding disabled (--no-agent-forwarding)")
	}
	if *maxSessions > 0 {
		mgr.SetMaxSessions(*maxSessions)
		log.Printf("session limit: %d (--max-sessions)", *maxSessions)
	}

	// 無停止再起動（self re-exec）からの復元: 旧プロセスが継承させた状態 fd があれば、
	// セッション（PTY・リングバッファ・QUIC ソケット）をここで引き継ぐ。
	// 共有 transport も状態に含まれるため、復元された場合は InitSharedUDP をスキップする。
	if fdStr := os.Getenv(session.HandoverEnvVar); fdStr != "" {
		os.Unsetenv(session.HandoverEnvVar) // 以後生成する子プロセスへ漏らさない
		if fd, err := strconv.Atoi(fdStr); err != nil {
			log.Printf("handover: invalid %s=%q: %v (starting fresh)", session.HandoverEnvVar, fdStr, err)
		} else {
			stateFile := os.NewFile(uintptr(fd), "handover-state")
			n, err := mgr.RestoreHandover(stateFile)
			stateFile.Close()
			if err != nil {
				log.Printf("handover: restore failed: %v (starting fresh, %d sessions restored)", err, n)
			} else {
				log.Printf("handover: restored %d session(s) from previous process", n)
			}
		}
	}

	// 固定 UDP ポート指定時は共有 QUIC transport を1つ起動し、全セッションを1ポートに相乗りさせる
	// （固定ポートを port-forward した運用向け）。未指定なら各セッションが per-session で QUIC を起動。
	// 無停止再起動で共有 transport を引き継いだ場合は不要（ソケットごと復元済み）。
	if *udpPort > 0 && !mgr.IsSharedUDPEnabled() {
		if err := mgr.InitSharedUDP(*udpPort, *ipv4Only); err != nil {
			log.Fatalf("failed to initialize shared QUIC transport: %v", err)
		}
	}

	// デフォルトでUnix Socketを使用
	if *listenUnix == "" {
		socketPath, err := netx.GetDefaultSocketPath()
		if err != nil {
			log.Fatalf("failed to get default socket path: %v", err)
		}
		*listenUnix = socketPath
	}

	// 既存のソケットが使用中かチェック
	if _, err := os.Stat(*listenUnix); err == nil {
		// ソケットファイルが存在する場合、接続を試みる
		testConn, err := net.Dial("unix", *listenUnix)
		if err == nil {
			// 接続成功 = 既に tezzerd が起動中
			testConn.Close()
			log.Fatalf("tezzerd is already running on socket: %s", *listenUnix)
		}
		// 接続失敗 = 古いソケットファイルなので削除
		if err := os.Remove(*listenUnix); err != nil {
			log.Fatalf("failed to remove stale socket: %v", err)
		}
	}

	listener, err := net.Listen("unix", *listenUnix)
	if err != nil {
		log.Fatalf("failed to listen on unix socket: %v", err)
	}
	defer listener.Close()
	defer os.Remove(*listenUnix)

	// ソケットファイルのパーミッションを設定（所有者のみアクセス可能）
	if err := os.Chmod(*listenUnix, 0600); err != nil {
		log.Fatalf("failed to chmod socket: %v", err)
	}

	// シグナルハンドリング（SIGINT/SIGTERM時にソケットを削除、SIGUSR1でデバッグトグル、
	// SIGUSR2 で無停止再起動 = self re-exec）
	shutdownCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGUSR2 {
				// 無停止再起動: 状態をシリアライズして自分自身を exec で置き換える。
				// 成功時はこのプロセスごと消える（この行以降は実行されない）。
				log.Printf("SIGUSR2: starting graceful handover (self re-exec)")
				if err := performHandover(mgr); err != nil {
					log.Printf("handover failed, continuing without restart: %v", err)
				}
			} else if sig == syscall.SIGUSR1 {
				// デバッグ出力をトグル
				old := atomic.LoadInt32(&debugEnabled)
				var newVal int32
				if old == 0 {
					newVal = 1
				}
				atomic.StoreInt32(&debugEnabled, newVal)
				enabled := newVal == 1
				session.SetDebugEnabled(enabled)
				// 全セッションのデバッグフラグも更新（UDP Managerを含む）
				mgr.SetDebugForAllSessions(enabled)
				status := "OFF"
				if enabled {
					status = "ON"
				}
				log.Printf("Debug output toggled: %s", status)
			} else {
				// SIGINT/SIGTERM
				log.Printf("received signal, cleaning up...")
				signal.Stop(sigCh) // これ以上シグナルを受け取らない
				listener.Close()
				os.Remove(*listenUnix)
				mgr.Close() // 共有UDP Managerを含むリソースをクリーンアップ
				close(shutdownCh)
				return // goroutineを終了
			}
		}
	}()

	// PTY 終了済みセッションの回収は Manager の cleanup ticker（60秒間隔・猶予30秒、
	// manager.go の cleanupClosedSessions）が担う。

	log.Printf("tezzerd: listening on unix socket: %s", *listenUnix)

	for {
		conn, err := listener.Accept()
		if err != nil {
			// シャットダウン中の場合は、accept errorをログ出力せずに終了
			select {
			case <-shutdownCh:
				// graceful shutdown
				return
			default:
				// "use of closed network connection" エラーは終了処理中なのでログ抑制
				if !errors.Is(err, net.ErrClosed) && err.Error() != "accept unix "+*listenUnix+": use of closed network connection" {
					log.Printf("accept error: %v", err)
				}
				return
			}
		}

		go handleConnection(conn, mgr, *stunServer, *ipv4Only, *udpPort)
	}
}

func handleConnection(conn net.Conn, mgr *session.Manager, stunServer string, ipv4Only bool, fixedUDPPort int) {
	defer conn.Close()

	log.Printf("new connection from %s", conn.RemoteAddr())

	// UID認証
	protocol := "UDS"
	remoteAddr := conn.RemoteAddr().String()

	peerUID, err := netx.GetPeerUID(conn)
	if err != nil {
		log.Printf("failed to get peer UID: %v", err)
		sendError(conn, proto.ErrUnauthorized, "Unix socket authentication failed")
		return
	}
	currentUID := uint32(os.Getuid())
	if peerUID != currentUID {
		log.Printf("authentication failed: peer UID %d != current UID %d", peerUID, currentUID)
		sendError(conn, proto.ErrUnauthorized, "UID mismatch")
		return
	}
	log.Printf("Unix socket authentication successful: UID %d", peerUID)

	// Expect HELLO
	frameData, err := netx.ReadFrame(conn)
	if err != nil {
		log.Printf("read hello error: %v", err)
		return
	}

	msg, err := proto.Decode(frameData)
	if err != nil {
		log.Printf("decode hello error: %v", err)
		return
	}

	helloMsg, ok := msg.(*proto.HelloMsg)
	if !ok {
		log.Printf("expected HELLO, got %T", msg)
		sendError(conn, proto.ErrBadRequest, "expected HELLO")
		return
	}

	log.Printf("client hello: %s (%dx%d)", helloMsg.ClientName, helloMsg.Cols, helloMsg.Rows)

	// Send WELCOME
	welcomeMsg := proto.WelcomeMsg{
		Type:       "WELCOME",
		ServerName: "tezzerd",
	}

	welcomeData, err := msgpack.Marshal(welcomeMsg)
	if err != nil {
		log.Printf("marshal welcome error: %v", err)
		return
	}

	if err := netx.WriteFrame(conn, welcomeData); err != nil {
		log.Printf("write welcome error: %v", err)
		return
	}

	// Wait for CREATE_SESSION or ATTACH_SESSION
	frameData, err = netx.ReadFrame(conn)
	if err != nil {
		log.Printf("read session command error: %v", err)
		return
	}

	msg, err = proto.Decode(frameData)
	if err != nil {
		log.Printf("decode session command error: %v", err)
		return
	}

	var sess *session.Session
	var client *session.Client

	switch m := msg.(type) {
	case *proto.ListSessionsMsg:
		// セッション一覧を返す
		sessions := mgr.GetAllSessions()
		if isDebugEnabled() {
			log.Printf("LIST_SESSIONS: total sessions=%d", len(sessions))
		}
		sessInfos := make([]proto.SessionInfo, len(sessions))
		for i, s := range sessions {
			if isDebugEnabled() {
				log.Printf("LIST_SESSIONS: session %s has %d clients", s.ID, s.GetClientCount())
			}
			sessInfos[i] = sessionProtoInfo(s)
		}
		listMsg := proto.SessionsListMsg{
			Type:     "SESSIONS_LIST",
			Sessions: sessInfos,
		}
		listData, err := msgpack.Marshal(listMsg)
		if err != nil {
			log.Printf("marshal sessions list error: %v", err)
			return
		}
		if err := netx.WriteFrame(conn, listData); err != nil {
			log.Printf("write sessions list error: %v", err)
		}
		return

	case *proto.GetSessionInfoMsg:
		// セッション情報を返す
		sess, err := mgr.GetSession(m.SessionID)
		if err != nil {
			sendError(conn, proto.ErrNoSuchSession, err.Error())
			return
		}

		infoMsg := proto.SessionInfoMsg{
			Type:    "SESSION_INFO",
			Session: sessionProtoInfo(sess),
		}
		infoData, err := msgpack.Marshal(infoMsg)
		if err != nil {
			log.Printf("marshal session info error: %v", err)
			return
		}
		if err := netx.WriteFrame(conn, infoData); err != nil {
			log.Printf("write session info error: %v", err)
		}
		return

	case *proto.KillSessionMsg:
		// セッションを終了する
		killSessionAndRespond(conn, mgr, m.SessionID)
		return

	case *proto.WaitSessionMsg:
		// セッションのコマンド終了を待つ（-wait）
		waitSessionAndRespond(conn, mgr, m.SessionID)
		return

	case *proto.CreateSessionMsg:
		if m.Name != "" {
			if err := proto.ValidateSessionName(m.Name); err != nil {
				sendError(conn, proto.ErrBadRequest, err.Error())
				return
			}
		}
		log.Printf("creating session: cmd=%s args=%v name=%q", m.Cmd, m.Args, m.Name)
		sess, err = mgr.CreateSession(m.Name, m.Cmd, m.Args, m.Env, m.Cwd, m.Rows, m.Cols, ipv4Only, fixedUDPPort, m.AgentForward)
		if err != nil {
			log.Printf("create session error: %v", err)
			if errors.Is(err, session.ErrDuplicateName) {
				sendError(conn, proto.ErrDuplicateName, err.Error())
				return
			}
			if errors.Is(err, session.ErrTooManySessions) {
				sendError(conn, proto.ErrTooManySessions, err.Error())
				return
			}
			sendError(conn, proto.ErrInternal, fmt.Sprintf("failed to create session: %v", err))
			return
		}

		stunAddrs, localAddr := resolveUDPAddrs(sess, stunServer, ipv4Only)

		udpSessionID := sess.GetUDPSessionID()
		if debugEnabled != 0 {
			log.Printf("session %s: sending UDPSessionID=%x (len=%d)", sess.ID, udpSessionID, len(udpSessionID))
		}
		createdMsg := proto.SessionCreatedMsg{
			Type:         "SESSION_CREATED",
			SessionID:    sess.ID,
			UDPEnabled:   sess.IsUDPEnabled(),
			UDPPort:      sess.GetUDPPort(),
			UDPKey:       sess.GetUDPKey(),
			UDPSessionID: udpSessionID,
			STUNAddrs:    stunAddrs,
			LocalAddr:    localAddr,
		}

		createdData, err := msgpack.Marshal(createdMsg)
		if err != nil {
			log.Printf("marshal session created error: %v", err)
			return
		}
		if err := netx.WriteFrame(conn, createdData); err != nil {
			log.Printf("write session created error: %v", err)
			return
		}

		// QUIC 確立前は UDS への PTY 出力配信を抑制する。
		// PTY と並行して QUIC ハンドシェイクが走るため、実際の遅延は最小限。
		// 起動前の PTY 出力は outputChunks に蓄積され、QUIC onResync で一括配信される。
		// こうすることで DA クエリが UDS と QUIC の両経路で端末エミュレータに届いて
		// DA 応答が二重に戻り漏洩する問題を防ぐ。
		sess.BeginQuicPendingMode()
		client = sess.AttachClient(0, protocol, remoteAddr, 0)

		go func() {
			const quicReadyTimeout = 15 * time.Second
			timer := time.NewTimer(quicReadyTimeout)
			defer timer.Stop()
			select {
			case <-sess.QuicReadyCh():
				if err := sess.StartProcess(); err != nil {
					log.Printf("session %s: start process error: %v", sess.ID, err)
				}
			case <-sess.Done():
				// 他経路（detach・kill 等）で既に Close 済み。タイムアウト処理は不要。
			case <-timer.C:
				log.Printf("session %s: QUIC not established within %v, closing session", sess.ID, quicReadyTimeout)
				sendError(conn, "QUIC_TIMEOUT", "QUIC connection not established in time")
				sess.Close()
			}
		}()

	case *proto.AttachSessionMsg:
		log.Printf("attaching to session: id=%s fromSeq=%d", m.SessionID, m.FromSeq)
		sess, err = mgr.GetSession(m.SessionID)
		if err != nil {
			log.Printf("get session error: %v", err)
			sendError(conn, proto.ErrNoSuchSession, err.Error())
			return
		}

		// Resize if needed（0x0 は「リサイズしない」= -N の転送専用 attach）
		if m.Rows > 0 && m.Cols > 0 {
			if err := sess.Resize(m.Rows, m.Cols); err != nil {
				log.Printf("resize error: %v", err)
			}
		}

		// Attach client (UDP情報はAttachSessionMsgから取得)
		client = sess.AttachClient(m.FromSeq, protocol, remoteAddr, m.ClientID)

		// Send SESSION_CREATED (attach成功の通知として)
		stunAddrs, localAddr := resolveUDPAddrs(sess, stunServer, ipv4Only)

		attachUDPSessionID := sess.GetUDPSessionID()
		if debugEnabled != 0 {
			log.Printf("session %s: sending UDPSessionID=%x (len=%d) on attach", sess.ID, attachUDPSessionID, len(attachUDPSessionID))
		}
		attachedMsg := proto.SessionCreatedMsg{
			Type:         "SESSION_CREATED",
			SessionID:    sess.ID,
			UDPEnabled:   sess.IsUDPEnabled(),
			UDPPort:      sess.GetUDPPort(),
			UDPKey:       sess.GetUDPKey(),
			UDPSessionID: attachUDPSessionID, // 共有UDPモード時のみ設定される
			STUNAddrs:    stunAddrs,
			LocalAddr:    localAddr,
			PTYClosed:    sess.IsPTYClosed(),
		}

		attachedData, err := msgpack.Marshal(attachedMsg)
		if err != nil {
			log.Printf("marshal session attached error: %v", err)
			return
		}
		if err := netx.WriteFrame(conn, attachedData); err != nil {
			log.Printf("write session attached error: %v", err)
			return
		}

	default:
		log.Printf("unexpected message type: %T", msg)
		sendError(conn, proto.ErrBadRequest, "expected CREATE_SESSION or ATTACH_SESSION")
		return
	}

	// Start output writer
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-client.Done:
				// クライアントがDetachされた場合は終了
				return
			case frameData, ok := <-client.OutCh:
				if !ok {
					return
				}
				if err := netx.WriteFrame(conn, frameData); err != nil {
					log.Printf("write frame error: %v", err)
					return
				}
			}
		}
	}()

	// クライアント識別用の表示名を準備
	clientDisplay := ""
	remoteAddrObj := conn.RemoteAddr()
	if remoteAddrObj != nil {
		addrStr := remoteAddrObj.String()
		// Unix Socketの場合は "@" や空文字になることがある
		if addrStr != "" && addrStr != "@" {
			clientDisplay = addrStr
		}
	}

	// Unix Socketまたはアドレス取得失敗の場合
	if clientDisplay == "" {
		if peerUID, err := netx.GetPeerUID(conn); err == nil {
			clientDisplay = fmt.Sprintf("unix-socket(UID:%d)", peerUID)
		} else {
			clientDisplay = "unix-socket"
		}
	}

	// Read input, resize, ping messages
	for {
		frameData, err := netx.ReadFrame(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("client %s: session %s: connection error: %v\n", clientDisplay, sess.ID, err)
			} else {
				log.Printf("client %s: session %s: disconnected\n", clientDisplay, sess.ID)
			}
			break
		}

		msg, err := proto.Decode(frameData)
		if err != nil {
			log.Printf("decode error: %v", err)
			continue
		}

		switch m := msg.(type) {
		case *proto.InputMsg:
			if err := sess.WriteInput(m.Data); err != nil {
				// PTYが閉じられている場合はログ出力しない（正常な終了状態）
				if !sess.IsPTYClosed() {
					log.Printf("write input error: %v", err)
				}
			}

		case *proto.ResizeMsg:
			if err := sess.Resize(m.Rows, m.Cols); err != nil {
				log.Printf("resize error: %v", err)
			}

		case *proto.UDPClientInfoMsg:
			// クライアント ClientID を受信 → 出力ファンアウト対象として登録。
			if err := sess.RegisterUDPClient(m.ClientID); err != nil {
				log.Printf("session %s: failed to register UDP client: %v", m.SessionID, err)
			}

		case *proto.PingMsg:
			pongMsg := proto.PongMsg{
				Type:  "PONG",
				Nonce: m.Nonce,
			}
			pongData, err := msgpack.Marshal(pongMsg)
			if err != nil {
				log.Printf("marshal pong error: %v", err)
				continue
			}
			if err := netx.WriteFrame(conn, pongData); err != nil {
				log.Printf("write pong error: %v", err)
				break
			}

		case *proto.GetServerMetaMsg:
			// サーバーのメタ情報を返す
			serverInstanceID := mgr.GetServerInstanceID()
			metaMsg := proto.ServerMetaMsg{
				Type:             "SERVER_META",
				ServerBuildID:    version.GetVersion(),
				ServerBuildTime:  version.GetBuildTime(),
				ServerInstanceID: serverInstanceID[:],
			}

			metaData, err := msgpack.Marshal(metaMsg)
			if err != nil {
				log.Printf("marshal server meta error: %v", err)
				continue
			}
			if err := netx.WriteFrame(conn, metaData); err != nil {
				log.Printf("write server meta error: %v", err)
				break
			}

		case *proto.KillSessionMsg:
			// セッションを終了する（セッションループ内からの処理）
			log.Printf("session %s: kill request received from client", m.SessionID)
			killSessionAndRespond(conn, mgr, m.SessionID)
			return

		default:
			log.Printf("unexpected message type: %T", msg)
		}
	}

	// Cleanup
	sess.DetachClient(client.ID)
	// writer goroutine は client.Done しか見ておらず、UDS write でブロック中は
	// select に戻れない。関数末尾の defer conn.Close() を待っていると、その
	// defer 自体が <-writerDone の後でしか走らないため詰まってしまう。
	// ここで先に conn を閉じて、ブロック中の write を中断させる。
	_ = conn.Close()
	<-writerDone
	log.Printf("client %s: session %s detached", clientDisplay, sess.ID)
}

func sendError(conn net.Conn, code, message string) {
	errMsg := proto.ErrorMsg{
		Type:    "ERROR",
		Code:    code,
		Message: message,
	}
	errData, _ := msgpack.Marshal(errMsg)
	netx.WriteFrame(conn, errData)
}

// sessionProtoInfo は Session から proto.SessionInfo を組み立てる（LIST_SESSIONS・
// GET_SESSION_INFO で共通）。旧実装では LIST_SESSIONS 側だけ OutputBufferBytes 等の
// リングバッファ統計が、GET_SESSION_INFO 側だけ LastDetachedAt/LastUDPAddr が
// 欠けているという非対称性があったが、統合により解消した。
func sessionProtoInfo(s *session.Session) proto.SessionInfo {
	clientInfos := s.GetClientInfos()
	protoClientInfos := clientInfosWithStats(clientInfos, s.GetClientSendBufferStats())

	var lastDetachedAt int64
	if t := s.GetLastDetachedAt(); !t.IsZero() {
		lastDetachedAt = t.Unix()
	}

	outputStats := s.GetOutputBufferStats()
	var oldestChunkTime int64
	if !outputStats.OldestChunkTime.IsZero() {
		oldestChunkTime = outputStats.OldestChunkTime.Unix()
	}

	var lastOutputAt, lastInputAt int64
	if t := s.GetLastOutputAt(); !t.IsZero() {
		lastOutputAt = t.Unix()
	}
	if t := s.GetLastInputAt(); !t.IsZero() {
		lastInputAt = t.Unix()
	}

	return proto.SessionInfo{
		SessionID:          s.ID,
		Name:               s.Name,
		Cmd:                s.Cmd,
		Rows:               s.Rows,
		Cols:               s.Cols,
		CreatedAt:          s.CreatedAt.Unix(),
		ClientCount:        s.GetClientCount(),
		Clients:            protoClientInfos,
		UDPEnabled:         s.IsUDPEnabled(),
		UDPPort:            s.GetUDPPort(),
		PTYClosed:          s.IsPTYClosed(),
		LastDetachedAt:     lastDetachedAt,
		LastUDPAddr:        s.GetLastUDPAddr(),
		LastOutputAt:       lastOutputAt,
		LastInputAt:        lastInputAt,
		OutputChunks:       outputStats.ChunkCount,
		OutputBufferBytes:  outputStats.TotalBytes,
		OldestChunkTime:    oldestChunkTime,
		OutputColdSegments: outputStats.ColdSegments,
		OutputColdBytes:    outputStats.ColdBytes,
		OutputColdRawBytes: outputStats.ColdRawBytes,
	}
}

// clientInfosWithStats は session.ClientInfo を対応する送信統計（backpressure 観測・
// 転送統計を含む）とマージして proto.ClientInfo のスライスに変換する。
// LIST_SESSIONS・GET_SESSION_INFO で共通（旧実装では LIST_SESSIONS 側だけ
// SendBufferBytes/SlowOutputWrites/MaxOutputWriteMs が未設定という非対称性が
// あったが、統合により解消した）。
func clientInfosWithStats(clientInfos []session.ClientInfo, sendStats []session.ClientSendBufferStats) []proto.ClientInfo {
	sendStatsMap := make(map[uint16]session.ClientSendBufferStats, len(sendStats))
	for _, st := range sendStats {
		sendStatsMap[st.ClientID] = st
	}
	result := make([]proto.ClientInfo, len(clientInfos))
	for i, ci := range clientInfos {
		result[i] = proto.ClientInfo{
			ID:           ci.ID,
			Protocol:     ci.Protocol,
			RemoteAddr:   ci.RemoteAddr,
			UDPClientID:  ci.UDPClientID,
			UDPAddresses: ci.UDPAddresses,
		}
		if st, ok := sendStatsMap[ci.UDPClientID]; ok {
			result[i].SendBufferBytes = st.TotalBytes
			if !st.LastSeen.IsZero() {
				result[i].LastSeen = st.LastSeen.Unix()
			}
			result[i].QUICRemoteAddr = st.QUICRemoteAddr
			result[i].SlowOutputWrites = st.SlowWrites
			result[i].MaxOutputWriteMs = st.MaxWriteMs
			result[i].OutputStallEpisodes = st.StallEpisodes
			result[i].OutputStallMs = st.CurrentStallMs
			result[i].ForwardsActive = int(st.ForwardsActive)
			result[i].ForwardsOpened = st.ForwardsOpened
			result[i].ForwardBytesToTarget = st.ForwardBytesToTarget
			result[i].ForwardBytesFromTarget = st.ForwardBytesFromTarget
		}
	}
	return result
}

// killSessionAndRespond はセッションを削除し、成功時は SESSION_KILLED を返す
// （失敗時は sendError）。KillSessionMsg は初回コマンドとセッションループ内の
// 両方から届きうるため、ここに共通化する。
func killSessionAndRespond(conn net.Conn, mgr *session.Manager, sessionID string) {
	if err := mgr.DeleteSession(sessionID); err != nil {
		sendError(conn, proto.ErrNoSuchSession, err.Error())
		return
	}
	killedMsg := proto.SessionKilledMsg{
		Type:      "SESSION_KILLED",
		SessionID: sessionID,
	}
	killedData, err := msgpack.Marshal(killedMsg)
	if err != nil {
		log.Printf("marshal session killed error: %v", err)
		return
	}
	if err := netx.WriteFrame(conn, killedData); err != nil {
		log.Printf("write session killed error: %v", err)
	}
	log.Printf("session %s killed by client request", sessionID)
}

// waitSessionAndRespond は WAIT_SESSION を処理する。セッションの終了（Done）まで
// この接続の goroutine 上でブロックし、終了したら SESSION_CLOSED NOTE（exit code 付き）
// を返す。待機中にクライアント側が切断した場合（Ctrl-C 等）はそのまま終了する。
func waitSessionAndRespond(conn net.Conn, mgr *session.Manager, sessionID string) {
	sess, err := mgr.GetSession(sessionID)
	if err != nil {
		sendError(conn, proto.ErrNoSuchSession, err.Error())
		return
	}
	log.Printf("session %s: wait request received", sessionID)

	// waiter は以後何も送ってこないので、ReadFrame がエラーを返したら切断とみなす。
	// セッションより先に waiter が消えたときに goroutine を残さないための監視。
	connClosed := make(chan struct{})
	go func() {
		_, _ = netx.ReadFrame(conn)
		close(connClosed)
	}()

	// Done() はプロセス回収（exit code 記録）後に close される（ptyReader の defer 参照）
	select {
	case <-sess.Done():
	case <-connClosed:
		log.Printf("session %s: waiter disconnected", sessionID)
		return
	}

	noteMsg := proto.NoteMsg{
		Type: "NOTE",
		Kind: "SESSION_CLOSED",
		Msg:  "PTY session has ended",
	}
	if code := sess.ExitCode(); code >= 0 {
		noteMsg.ExitCode = &code
	}
	noteData, err := msgpack.Marshal(noteMsg)
	if err != nil {
		log.Printf("marshal wait notification error: %v", err)
		return
	}
	if err := netx.WriteFrame(conn, noteData); err != nil {
		log.Printf("write wait notification error: %v", err)
	}
}

// resolveUDPAddrs はセッションのSTUNアドレス候補（IPv4/IPv6、取得できた分）と
// ローカルアドレスを解決する。初回接続の候補を増やすため両familyを問い合わせる。
func resolveUDPAddrs(sess *session.Session, stunServer string, ipv4Only bool) (stunAddrs []string, localAddr string) {
	if !sess.IsUDPEnabled() {
		return
	}

	if stunServer != "" {
		if ip := queryStunMappedIP(sess.ID, "udp4"); ip != nil {
			stunAddrs = append(stunAddrs, net.JoinHostPort(ip.String(), strconv.Itoa(sess.GetUDPPort())))
		}
		if !ipv4Only {
			if ip := queryStunMappedIP(sess.ID, "udp6"); ip != nil {
				stunAddrs = append(stunAddrs, net.JoinHostPort(ip.String(), strconv.Itoa(sess.GetUDPPort())))
			}
		}
	}

	if localIP := getLocalAddr(); localIP != "" {
		localAddr = fmt.Sprintf("%s:%d", localIP, sess.GetUDPPort())
	}
	return
}

// queryStunMappedIP は指定family（"udp4"/"udp6"）でGoogle/Cloudflareの STUN
// サーバーに問い合わせ、マッピングされたIPを返す。失敗時はnil（呼び出し側は
// そのfamilyを候補から単純に省く）。
func queryStunMappedIP(sessID, network string) net.IP {
	stunClient := stun.NewClient("stun.l.google.com:19302")
	stunClient.Network = network
	natType, mappedAddr, err := stunClient.DetectNATType(
		"stun.l.google.com:19302",
		"stun.cloudflare.com:3478",
	)
	if err != nil {
		log.Printf("session %s: STUN query (%s) failed (continuing without it): %v", sessID, network, err)
		return nil
	}

	natTypeName := "Unknown"
	switch natType {
	case stun.NATTypeFullCone:
		natTypeName = "Full Cone (or Port Restricted)"
	case stun.NATTypeSymmetric:
		natTypeName = "Symmetric"
	}
	log.Printf("session %s: NAT type (%s): %s, mapped IP: %s", sessID, network, natTypeName, mappedAddr.IP)
	return mappedAddr.IP
}

// getLocalAddr は優先的なローカルIPアドレスを取得
// LAN内接続時にクライアントが使用できるアドレスを返す
func getLocalAddr() string {
	// UDP接続を使ってローカルアドレスを特定（実際には接続しない）
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}
