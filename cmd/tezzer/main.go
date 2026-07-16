package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/qtransport"
	"github.com/kuriyama/tezzer/internal/stun"
	"github.com/kuriyama/tezzer/internal/termui"
	"github.com/kuriyama/tezzer/internal/transport"
	"github.com/kuriyama/tezzer/internal/version"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"
)

var (
	// errDetached はユーザーによる意図的なdetach（正常終了）
	errDetached = errors.New("detached by user")
)

func main() {
	addrUnix := flag.String("addr-unix", "", "Unix Domain Socket path (default: $XDG_RUNTIME_DIR/tezzer.sock or ~/.tezzer/tezzer.sock)")
	sessionID := flag.String("session", "", "session id to attach (empty => create)")
	resume := flag.Bool("resume", false, "attach to the most recent session (shortcut for -session <latest>)")
	name := flag.String("name", "", "named session: attach if a session with this name exists, otherwise create one")
	cmd := flag.String("cmd", "/bin/bash", "command to run when creating session")
	list := flag.Bool("list", false, "list all sessions and exit")
	info := flag.String("info", "", "show info for session and exit")
	stats := flag.String("stats", "", "show stats for session and exit (use Ctrl-^ s to generate)")
	logs := flag.String("logs", "", "show client log for session and exit")
	jsonOut := flag.Bool("json", false, "output in JSON format (use with -list, -info, -stats)")
	kill := flag.String("kill", "", "kill session and exit")
	wait := flag.Bool("wait", false, "wait for the session command to exit, then exit with its code (requires -session, -resume, or -name)")
	peek := flag.Bool("peek", false, "read-only attach: view output but never send input or resize (requires -session, -resume, or -name)")
	escapeKey := flag.String("escape-key", "^", "escape key prefix (default: Ctrl-^, use '^', '~', etc.)")
	var localForwards stringListFlag
	flag.Var(&localForwards, "L", "local port forward [bind:]port:host:hostport (bind: loopback only; repeatable)")
	noPTY := flag.Bool("N", false, "no PTY I/O: attach for port forwarding only (requires -L and an existing session via -session/-resume/-name)")
	agentForward := flag.Bool("A", false, "forward local SSH agent ($SSH_AUTH_SOCK) to the session (create-time only)")
	ipv4Only := flag.Bool("ipv4-only", false, "use IPv4 only for STUN and UDP connections")
	udpPort := flag.Int("udp-port", 0, "fixed UDP port for client (default: auto-assign)")
	flag.Parse()

	// 環境変数 TEZZER_SOCKET のチェック（tezzer-ssh スクリプト用）
	envSocket := os.Getenv("TEZZER_SOCKET")
	if envSocket != "" && *addrUnix == "" {
		*addrUnix = envSocket
	}

	// デフォルトでUnix Socketを使用
	if *addrUnix == "" {
		socketPath, err := netx.GetDefaultSocketPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to get default socket path: %v\n", err)
			os.Exit(1)
		}
		*addrUnix = socketPath
	}

	connectAddr := *addrUnix

	// 管理コマンドの処理
	if *list {
		if err := listSessions(connectAddr, *jsonOut); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *info != "" {
		if err := showSessionInfo(connectAddr, *info, *jsonOut); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *stats != "" {
		if err := showSessionStats(*stats, *jsonOut); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *logs != "" {
		if err := showSessionLogs(*logs); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *kill != "" {
		if err := killSession(connectAddr, *kill); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// フラグの組み合わせチェック（サーバ問い合わせの前に行う）
	if *name != "" && (*sessionID != "" || *resume) {
		fmt.Fprintf(os.Stderr, "error: cannot use -name with -session or -resume\n")
		os.Exit(1)
	}
	// -N は既存セッションへの転送専用 attach。新規作成はしない
	// （トンネルのためだけに PTY/コマンドを起動しても仕方がないため）。
	if *noPTY {
		if len(localForwards) == 0 {
			fmt.Fprintf(os.Stderr, "error: -N requires at least one -L\n")
			os.Exit(1)
		}
		if *sessionID == "" && !*resume && *name == "" {
			fmt.Fprintf(os.Stderr, "error: -N requires -session, -resume, or -name to select an existing session\n")
			os.Exit(1)
		}
	}
	// -A は PTY の環境変数（SSH_AUTH_SOCK）の話であり、PTY を持たない -N attach には
	// 意味を持たない（docs/dev/agent-forwarding.md）。
	if *noPTY && *agentForward {
		fmt.Fprintf(os.Stderr, "error: -A cannot be combined with -N\n")
		os.Exit(1)
	}
	// -wait は attach しない終了待ちなので、attach 系のモード・オプションとは併用不可。
	// 新規作成もしない（終了を待つ対象が既存であることが前提）。
	if *wait {
		if *noPTY || *agentForward || len(localForwards) > 0 || *peek {
			fmt.Fprintf(os.Stderr, "error: -wait cannot be combined with -N, -A, -L, or -peek\n")
			os.Exit(1)
		}
		if *sessionID == "" && !*resume && *name == "" {
			fmt.Fprintf(os.Stderr, "error: -wait requires -session, -resume, or -name to select an existing session\n")
			os.Exit(1)
		}
	}
	// -peek は既存セッションの読み取り専用 attach。新規作成はしない。
	// -N（PTY なし）とは矛盾、-A は作成時のみ有効なので無意味 → どちらも併用不可。
	if *peek {
		if *noPTY || *agentForward {
			fmt.Fprintf(os.Stderr, "error: -peek cannot be combined with -N or -A\n")
			os.Exit(1)
		}
		if *sessionID == "" && !*resume && *name == "" {
			fmt.Fprintf(os.Stderr, "error: -peek requires -session, -resume, or -name to select an existing session\n")
			os.Exit(1)
		}
	}

	// -resume が指定された場合、最新のセッションIDを取得
	if *resume {
		if *sessionID != "" {
			fmt.Fprintf(os.Stderr, "error: cannot use both -resume and -session flags\n")
			os.Exit(1)
		}
		latestSessionID, err := getLatestSessionID(connectAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to get latest session: %v\n", err)
			os.Exit(1)
		}
		if latestSessionID == "" {
			fmt.Fprintf(os.Stderr, "error: no active sessions found\n")
			os.Exit(1)
		}
		*sessionID = latestSessionID
		fmt.Fprintf(os.Stderr, "Resuming latest session: %s\n", latestSessionID)
	}

	// -name: 名前で attach-or-create（tmux new -A -s / screen -S 相当）
	if *name != "" {
		if err := proto.ValidateSessionName(*name); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		namedSessionID, err := findSessionIDByName(connectAddr, *name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to look up session by name: %v\n", err)
			os.Exit(1)
		}
		if namedSessionID != "" {
			// 既存セッションに attach（未発見なら run() が名前付きで新規作成）
			*sessionID = namedSessionID
			if !*wait {
				fmt.Fprintf(os.Stderr, "Attaching to session %q: %s\n", *name, namedSessionID)
			}
		} else if *noPTY || *wait || *peek {
			fmt.Fprintf(os.Stderr, "error: no active session named %q\n", *name)
			os.Exit(1)
		}
	}

	// -wait: セッションのコマンド終了を待ち、exit code を伝搬して exit
	if *wait {
		code, err := waitSession(connectAddr, *sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(code)
	}

	// -L の指定を先に検証する（不正な指定は接続前に弾く）
	var forwards []forwardSpec
	for _, spec := range localForwards {
		sp, err := parseForwardSpec(spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		forwards = append(forwards, sp)
	}

	// エスケープキーのバイト値を計算
	escapeByte, err := parseEscapeKey(*escapeKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// -A: ローカルの $SSH_AUTH_SOCK が使える場合のみ解決する（create/attach どちらでも
	// Hello の AgentForward 判定に使う。既存セッションが -A 付きで作られていなければ
	// サーバ側で無視される）。
	agentSockPath := ""
	if *agentForward {
		agentSockPath = resolveLocalAgentSockPath()
		if agentSockPath == "" {
			fmt.Fprintf(os.Stderr, "warning: -A: $SSH_AUTH_SOCK not set or not a usable socket; agent forwarding will not work\n")
		}
	}

	exitCode, err := run(connectAddr, *sessionID, *cmd, *name, escapeByte, *ipv4Only, *udpPort, forwards, *noPTY, *agentForward, *peek, agentSockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// セッションプロセスの終了コードを伝搬する（ssh と同じ挙動。detach 等は 0）
	os.Exit(exitCode)
}

// parseEscapeKey はエスケープキー文字列を対応するバイト値に変換
func parseEscapeKey(key string) (byte, error) {
	if len(key) == 0 {
		return 0, fmt.Errorf("escape key cannot be empty")
	}

	// 最初の文字を使用
	ch := key[0]

	// Ctrl+キーの計算 (A-Z, [, \, ], ^, _, ?)
	if ch >= '@' && ch <= '_' {
		// Ctrl-@ = 0x00, Ctrl-A = 0x01, ..., Ctrl-^ = 0x1E, Ctrl-_ = 0x1F
		return ch - '@', nil
	}
	if ch == '?' {
		// Ctrl-? = 0x7F (DEL)
		return 0x7F, nil
	}
	if ch >= 'a' && ch <= 'z' {
		// 小文字も受け付ける (Ctrl-a = 0x01, ...)
		return ch - 'a' + 1, nil
	}

	return 0, fmt.Errorf("invalid escape key: %q (use A-Z, a-z, @, [, \\, ], ^, _, or ?)", key)
}

// escapeKeyDisplay はエスケープキーのバイト値から表示用文字列を生成
func escapeKeyDisplay(b byte) string {
	if b == 0x7F {
		return "Ctrl-?"
	}
	if b <= 0x1F {
		// Ctrl-@ から Ctrl-_ まで
		ch := '@' + b
		return fmt.Sprintf("Ctrl-%c", ch)
	}
	return fmt.Sprintf("0x%02x", b)
}

// UDPInfo はUDP接続情報を保持
type UDPInfo struct {
	Port      int
	Key       []byte
	SessionID []byte   // 共有UDPモード時のSessionID（nilの場合はセッションIDからハッシュを生成）
	STUNAddrs []string // サーバーのSTUN経由アドレス候補（NAT越え用、family別）
	LocalAddr string   // サーバーのローカルアドレス（LAN内接続用）
}

// udpInfoFromSessionCreated は SESSION_CREATED 応答（create/attach 共通）から
// UDP 接続情報を取り出す。UDP 無効なら nil。
func udpInfoFromSessionCreated(m *proto.SessionCreatedMsg) *UDPInfo {
	if !m.UDPEnabled {
		return nil
	}
	return &UDPInfo{
		Port:      m.UDPPort,
		Key:       m.UDPKey,
		SessionID: m.UDPSessionID, // 共有UDPモード時のみ設定される
		STUNAddrs: m.STUNAddrs,
		LocalAddr: m.LocalAddr,
	}
}

// buildUDPCandidateAddrs はサーバー情報とクライアント STUN アドレス候補から
// 接続候補リストを生成する純粋関数。startUDPManager と候補リフレッシャーの両方で使用。
func buildUDPCandidateAddrs(udpInfo *UDPInfo, clientSTUNAddrs []string) []string {
	var candidates []string

	// クライアント・サーバー双方に STUN アドレスがあり、どの family の組でも
	// IP が一致しなければリモート接続とみなす（同一ホストなら少なくとも1つの
	// family で一致するはず）。
	isRemoteConnection := false
	if len(clientSTUNAddrs) > 0 && len(udpInfo.STUNAddrs) > 0 {
		isRemoteConnection = true
		for _, ca := range clientSTUNAddrs {
			clientIP, _, _ := net.SplitHostPort(ca)
			if clientIP == "" {
				continue
			}
			for _, sa := range udpInfo.STUNAddrs {
				serverIP, _, _ := net.SplitHostPort(sa)
				if serverIP != "" && clientIP == serverIP {
					isRemoteConnection = false
				}
			}
		}
	}

	if !isRemoteConnection && serverIsOnSameHost(udpInfo.LocalAddr) {
		candidates = append(candidates, fmt.Sprintf("127.0.0.1:%d", udpInfo.Port))
	}
	if !isRemoteConnection && udpInfo.LocalAddr != "" && !strings.HasPrefix(udpInfo.LocalAddr, "127.") {
		candidates = append(candidates, udpInfo.LocalAddr)
	}
	for _, addr := range udpInfo.STUNAddrs {
		if addr != "" {
			candidates = append(candidates, addr)
		}
	}
	// 候補ゼロ（完全オフライン環境などで LocalAddr も STUN も取れない）の場合、
	// 最後の頼みとして loopback を試す。ローカル利用(UDS 直結)ならこれで繋がる。
	// サーバが実際には別ホストでも mTLS（K の pinning）検証で安全に失敗するだけ。
	if len(candidates) == 0 && !isRemoteConnection {
		candidates = append(candidates, fmt.Sprintf("127.0.0.1:%d", udpInfo.Port))
	}
	return candidates
}

// run はセッションへ接続して端末セッション（または -N の転送専用 attach）を実行する。
// noPTY（-N）のときは端末を触らず（raw mode・stdin・出力表示なし）、転送だけを行う。
// run はセッションへ接続し、終了コード（セッションプロセスのもの。不明・detach は 0）
// とエラーを返す。
func run(addr, sessionID, cmd, name string, escapeByte byte, ipv4Only bool, fixedUDPPort int, forwards []forwardSpec, noPTY, agentForward, readOnly bool, agentSockPath string) (int, error) {
	// 再帰接続チェック（tezzerセッション内からtezzerを起動した場合）
	if parentSession := os.Getenv("TEZZER_SESSION"); parentSession != "" {
		if sessionID != "" && sessionID == parentSession {
			// 同じセッションへの再帰接続は禁止
			return 0, fmt.Errorf("nested attach to the same session (%s) is not allowed\nUse %s . to detach first",
				sessionID, escapeKeyDisplay(escapeByte))
		}
		// 異なるセッションへの接続は警告のみ
		fmt.Fprintf(os.Stderr, "warning: running tezzer within tezzer session (%s)\n", parentSession)
	}

	// Get terminal size（-N は端末不要。0x0 はサーバ側で「リサイズしない」扱い）
	termFd := int(os.Stdin.Fd())
	width, height := 0, 0
	if !noPTY {
		var err error
		width, height, err = term.GetSize(termFd)
		if err != nil {
			return 0, fmt.Errorf("failed to get terminal size: %w", err)
		}
	}

	conn, err := dialAndHandshake(addr, width, height)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	// Create or attach session
	var actualSessionID string
	var udpInfo *UDPInfo // UDP情報（nilの場合はUDP無効）

	if sessionID == "" {
		// Create session
		// flag.Args() で -- 以降の引数を取得
		args := flag.Args()

		createdMsg, err := roundTrip[proto.SessionCreatedMsg](conn, proto.CreateSessionMsg{
			Type:         "CREATE_SESSION",
			Name:         name,
			Cmd:          cmd,
			Args:         args,
			Cols:         width,
			Rows:         height,
			Env:          map[string]string{"TERM": os.Getenv("TERM")},
			AgentForward: agentForward,
		})
		if err != nil {
			return 0, err
		}
		actualSessionID = createdMsg.SessionID

		// PTY終了チェック
		if createdMsg.PTYClosed {
			// InitialOutput があれば表示してから終了
			if len(createdMsg.InitialOutput) > 0 {
				os.Stdout.Write(createdMsg.InitialOutput)
			}
			// 非インタラクティブコマンドの場合、セッションを自動削除
			killMsg := proto.KillSessionMsg{
				Type:      "KILL_SESSION",
				SessionID: createdMsg.SessionID,
			}
			if killData, err := proto.Encode(killMsg); err == nil {
				netx.WriteFrame(conn, killData)
				// 応答は待たずに終了（接続は閉じられる）
			}
			return 0, nil // 正常終了（エラーではない。この経路に exit code は載らない）
		}

		udpInfo = udpInfoFromSessionCreated(createdMsg)

		// セッションIDはClient作成後にステータスメッセージで表示
		// （ここではまだclientインスタンスが存在しない）
	} else {
		// Attach to existing session
		if agentForward {
			// -A は作成時にのみ意味を持つ（既存セッションが -A なしで作られていれば
			// UDS リスナー自体が無く、reattach で後付けはできない）。
			fmt.Fprintf(os.Stderr, "note: -A only takes effect if this session was created with -A\n")
		}
		actualSessionID = sessionID
		log.Printf("attaching to session: %s", actualSessionID)

		// -peek はリモート PTY のサイズを変えない（0x0 = サーバ側で「リサイズしない」）
		attachCols, attachRows := width, height
		if readOnly {
			attachCols, attachRows = 0, 0
		}
		attachedMsg, err := roundTrip[proto.SessionCreatedMsg](conn, proto.AttachSessionMsg{
			Type:      "ATTACH_SESSION",
			SessionID: sessionID,
			FromSeq:   0,
			Cols:      attachCols,
			Rows:      attachRows,
		})
		if err != nil {
			return 0, err
		}

		// PTY終了チェック
		if attachedMsg.PTYClosed {
			return 0, fmt.Errorf("session PTY has already terminated")
		}

		udpInfo = udpInfoFromSessionCreated(attachedMsg)
	}

	// Create client
	client := &Client{
		conn:          conn,
		sessionID:     actualSessionID,
		termFd:        termFd,
		width:         width,
		height:        height,
		done:          make(chan struct{}),
		escapeByte:    escapeByte,
		ipv4Only:      ipv4Only,
		fixedUDPPort:  fixedUDPPort,
		errCh:         make(chan error, 1),
		udsAddr:       addr,
		forwards:      forwards,
		noPTY:         noPTY,
		readOnly:      readOnly,
		agentSockPath: agentSockPath,
	}
	client.sessionExitCode.Store(-1)
	// デバッグフラグの初期値（環境変数から）
	client.debugEnabled.Store(os.Getenv("TEZZER_DEBUG") != "")

	// セッション開始メッセージを設定（後でステータス行に表示される）
	if !noPTY {
		mode := ""
		if readOnly {
			mode = " [read-only]"
		}
		client.initialStatusMsg = fmt.Sprintf("[Tezzer] Session: %s%s | Use %s i for help", actualSessionID, mode, escapeKeyDisplay(escapeByte))
	}

	// Set terminal to raw mode（-N は端末を触らない）
	var oldState *term.State
	if !noPTY {
		var err error
		oldState, err = term.MakeRaw(termFd)
		if err != nil {
			return 0, fmt.Errorf("failed to set raw mode: %w", err)
		}
	}

	// クライアントログファイルを開く（失敗しても続行）
	if lf, err := openClientLogFile(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to open client log file: %v\n", err)
	} else {
		client.logFile = lf
		client.fileLog = newFileLogger(lf)
		client.fileLog.Printf("tezzer client started (pid=%d, commit=%s)", os.Getpid(), version.GitCommit)
		client.fileLog.Printf("session=%s server=%s", actualSessionID, addr)
		createSessionLogSymlink(actualSessionID, lf)
	}

	// raw mode中は\nを\r\nに変換するようlogを設定
	// TEZZER_DEBUG 有効時はファイルにもティー出力する
	// （-N は raw mode に入らないので通常の log 出力のまま）
	oldLogOutput := log.Writer()
	if !noPTY {
		stderrWriter := &termui.CRLFWriter{W: os.Stderr}
		if client.isDebugEnabled() && client.logFile != nil {
			log.SetOutput(io.MultiWriter(stderrWriter, client.logFile))
		} else {
			log.SetOutput(stderrWriter)
		}

		// StatusManager を初期化（raw mode中のステータス行表示）
		client.statusMgr = termui.NewStatusManager(
			func(msg string) {
				fmt.Fprintf(os.Stderr, "\033[s\033[H\033[7m%s\033[K\033[0m\033[u", msg)
			},
			func() {
				fmt.Fprintf(os.Stderr, "\033[s\033[H\033[K\033[u")
			},
		)
	}

	defer func() {
		// log出力を元に戻す
		log.SetOutput(oldLogOutput)
		// QUIC トランスポートをクリーンアップ
		if ct := client.transport(); ct != nil {
			_ = ct.Close()
		}
		if !noPTY {
			// StatusManagerをクリーンアップ
			client.statusMgr.Close()
			// カーソルを表示（念のため）
			fmt.Fprintf(os.Stderr, "\033[?25h")
			// ターミナル状態を復元
			term.Restore(termFd, oldState)
			// ONLCR を明示的に有効化（stty sane相当）
			restoreTerminalFlags(termFd)
		}
		// ログファイルを閉じる
		if client.logFile != nil {
			client.fileLog.Print("tezzer client exited")
			client.logFile.Close()
		}
	}()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM)

	// tmux 等の初期化シーケンス（DA2クエリ）をタイムリーに転送するため、
	// UDP セットアップ（STUN ルックアップ等）より先に goroutine を起動する。
	// -N は stdin を読まない（PTY 入出力なし。出力は writeOutput が破棄する）。
	if !noPTY {
		go client.handleStdin()
	}
	go client.handleServer()
	go client.sampleRTTHistory()

	// -L の listener を起動（トンネル定義はクライアントローカルなので reconnect を跨いで生きる）
	if len(forwards) > 0 {
		client.startForwardListeners()
		go client.warnIfForwardingUnsupported()
	}
	if client.agentSockPath != "" {
		go client.warnIfAgentForwardingUnsupported()
	}

	// UDP Managerを起動（UDP情報がある場合のみ）
	// STUN ルックアップを含むため数百ms かかることがある。
	// handleServer/handleStdin は既に起動済みで TCP で動作している。
	if udpInfo != nil {
		// UDP セットアップは Dial が最大 10s かかりうるためバックグラウンドで行い、
		// メインのイベントループや UDS 経路を妨げない。
		go func() {
			if err := client.startUDPManager(addr, udpInfo); err != nil {
				// 初回確立に失敗しても UDS-only で居座らせない（それは「ただの SSH」）。
				// UDS が生きている間、QUIC 確立を再試行する（高遅延でハンドシェイクが
				// 間に合わなかった等、後から繋がりうる）。
				client.setStatusMessage(fmt.Sprintf("UDP setup failed, retrying in background: %v", err))
				client.retryUDPManager(addr, udpInfo)
				return
			}
			client.setStatusMessage(fmt.Sprintf("UDP enabled: server port %d", udpInfo.Port))
			go client.handleUDPOutput()
		}()
	}

	// 初期ステータスメッセージを表示
	if client.initialStatusMsg != "" {
		client.setStatusMessage(client.initialStatusMsg)
	}
	if noPTY {
		log.Printf("attached to session %s (forward-only); Ctrl-C to exit", actualSessionID)
	}

	// Wait for signals or completion
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGWINCH {
				if noPTY {
					continue // -N はセッションの端末サイズに関与しない
				}
				// Handle resize
				newWidth, newHeight, err := term.GetSize(termFd)
				if err == nil && (newWidth != width || newHeight != height) {
					client.sendResize(newWidth, newHeight)
					width, height = newWidth, newHeight
				}
			} else {
				// Other signals - exit
				return client.exitCode(), nil
			}
		case err := <-client.errCh:
			// 異常終了（detachの場合は正常終了扱い）
			if errors.Is(err, errDetached) {
				return client.exitCode(), nil
			}
			return 0, err
		case <-client.done:
			// doneが閉じた場合、errChにエラーがあるかチェック
			select {
			case err := <-client.errCh:
				if errors.Is(err, errDetached) {
					return client.exitCode(), nil
				}
				return 0, err
			default:
				return client.exitCode(), nil
			}
		}
	}
}

// logEntry は通知メッセージのログエントリ
type logEntry struct {
	timestamp time.Time
	message   string
}

// maxLogEntries は保持する通知メッセージの最大数
const maxLogEntries = 20

// Client はクライアント状態を保持
type Client struct {
	conn      net.Conn
	sessionID string
	termFd    int
	width     int
	height    int
	done      chan struct{}
	doneOnce  sync.Once // doneチャネルを1回だけcloseするため
	// QUIC トランスポート（nil の場合は UDS のみ）。startUDPManager（メイン goroutine）が
	// 書き、handleServer/handleStdin など別 goroutine が読むため ctMu で保護する。
	ct   transport.ClientTransport
	ctMu sync.RWMutex

	escapeByte    byte          // エスケープキーのバイト値
	ipv4Only      bool          // IPv4のみ使用する場合true
	fixedUDPPort  int           // 固定UDPポート（0の場合は自動割り当て）
	escapePressed bool          // エスケープキーが押されたかどうか
	lastSeq       atomic.Uint64 // UDS 経路で最後に受信したシーケンス番号（欠番検出用）
	// renderedSeq は stdout へ描画済みの最大セッションオフセット。UDS の OutputMsg.Seq と
	// QUIC の出力 offset は同じ番号空間（サーバの currentSeq）なので、このカウンタ 1 つで
	// クロスパスの重複排除ができる（attach 時のバックログ二重描画の防止と、
	// QUIC 断中に UDS 経由の出力を安全に描画するためのフォールバック）。
	// 更新は renderMu 下で行う（読み取りはどこからでも可）。
	renderedSeq atomic.Uint64
	renderMu    sync.Mutex // renderedSeq の判定〜stdout 書き込みの直列化（UDS/QUIC 両経路）
	statusMgr   *termui.StatusManager

	lastOutputRecv atomic.Int64 // 最後に PTY 出力を受信した Unix ナノ秒（0 = 未受信）

	// RTT 履歴（Ctrl-^ i のスパークライン表示用、直近 rttHistorySize 件・古い順）
	rttHistoryMu sync.Mutex
	rttHistory   []float64

	errCh                 chan error  // 異常終了通知用（バッファ1）
	killing               atomic.Bool // Ctrl-^ q による kill 処理中（sessionGone をエラー扱いしない）
	sessionClosedNotified atomic.Bool // UDS 経由で SESSION_CLOSED を表示済み（QUIC 側の重複表示防止）
	// セッションプロセスの終了コード（-1 = 未受信）。SESSION_CLOSED 通知
	//（QUIC/UDS どちらの経路でも）で設定され、ssh と同様にクライアント自身の
	// 終了コードになる。
	sessionExitCode atomic.Int64

	// 動的デバッグ出力制御
	debugEnabled atomic.Value // bool を格納

	// サーバーメタ情報
	serverMeta *proto.ServerMetaMsg
	metaMu     sync.RWMutex

	// 通知メッセージログ（Ctrl-^ s 表示用）
	msgLogMu sync.RWMutex
	msgLog   []logEntry // 最新20件のログ

	// クライアントログファイル（常時書き出し）
	logFile *os.File
	fileLog *fileLogger

	udsAddr string     // UDS接続先アドレス
	connMu  sync.Mutex // conn更新時のロック

	// 初期ステータスメッセージ（接続直後に表示）
	initialStatusMsg string

	// ESC batching用（カーソルキー等の複数バイト入力をまとめる）
	escBatchActive bool
	escBatchBuf    []byte
	escBatchStart  time.Time

	// 通常入力batching用（ペースト時にまとめて送信）
	inputBatchBuf   []byte
	inputBatchStart time.Time

	// -L ポートフォワード
	forwards    []forwardSpec // 解析済みの -L 指定（不変）
	fwdActive   atomic.Int32  // アクティブな転送 TCP 接続数（Ctrl-^ i 表示用）
	fwdWarnMu   sync.Mutex
	lastFwdWarn time.Time // ステータス行への警告の間引き用

	// -N: 転送専用 attach（端末を触らない。PTY 出力は破棄、statusMgr は nil）
	noPTY bool

	// -peek: 読み取り専用 attach（誤爆防止）。入力・リサイズ・kill を一切送らない。
	// 出力表示とエスケープキー操作（detach/status 等）はそのまま使える
	readOnly bool
	roWarned bool // 入力破棄の初回通知済み（handleStdin goroutine 内でのみ触る）

	// -A: SSH agent forwarding。解決済みのローカル $SSH_AUTH_SOCK パス（不変。
	// 空文字なら -A 未指定 or ソケット不可＝Hello の AgentForward は false）。
	agentSockPath string
}

// exitCode はクライアントの終了コードを返す。セッションプロセスの終了コードを
// 受信していればそれ（ssh と同じ挙動）、未受信（detach 等）なら 0。
func (c *Client) exitCode() int {
	if v := c.sessionExitCode.Load(); v >= 0 {
		return int(v)
	}
	return 0
}

// noteSessionExitCode はセッション終了通知に載ってきた終了コードを記録する。
// 自分が kill した場合（Ctrl-^ q）は意図した終了なので 0 のままにする。
func (c *Client) noteSessionExitCode(code int) {
	if code >= 0 && !c.killing.Load() {
		c.sessionExitCode.Store(int64(code))
	}
}

// transport は現在の QUIC トランスポートを返す（nil の場合は UDS のみ）。ctMu で保護。
func (c *Client) transport() transport.ClientTransport {
	c.ctMu.RLock()
	defer c.ctMu.RUnlock()
	return c.ct
}

// setTransport は QUIC トランスポートを設定する。
func (c *Client) setTransport(t transport.ClientTransport) {
	c.ctMu.Lock()
	c.ct = t
	c.ctMu.Unlock()
}

// startUDPManager はUDP Managerを起動
func (c *Client) startUDPManager(udsAddr string, udpInfo *UDPInfo) error {
	// 接続試行するUDPアドレスのリストを決定（優先順位順）
	var candidateAddrs []string

	// クライアント識別子を生成（16-bitランダム値）
	// UDP Managerより先に生成する
	var clientIDBytes [2]byte
	if _, err := rand.Read(clientIDBytes[:]); err != nil {
		return fmt.Errorf("failed to generate client ID: %w", err)
	}
	clientID := binary.LittleEndian.Uint16(clientIDBytes[:])

	// クライアント側もSTUNで自分の公開アドレス候補を取得（リモート接続判定 =
	// 候補アドレス生成にクライアントローカルで使うだけで、サーバへは送らない）
	var clientSTUNAddrs []string
	if len(udpInfo.STUNAddrs) > 0 {
		clientSTUNAddrs = c.getClientSTUNAddrs()
		if len(clientSTUNAddrs) > 0 {
			c.setStatusMessage(fmt.Sprintf("client STUN address: %s", strings.Join(clientSTUNAddrs, ", ")))
		} else {
			c.setStatusMessage("warning: failed to get client STUN address")
		}
	}

	// clientID をサーバーに通知（UDS経由）。出力ファンアウト対象への登録に必要
	if err := c.sendUDPClientInfo(clientID); err != nil {
		c.setStatusMessage(fmt.Sprintf("warning: failed to send UDP client info: %v", err))
	}

	candidateAddrs = buildUDPCandidateAddrs(udpInfo, clientSTUNAddrs)

	// 候補アドレスを常時ログ＆通知（debug 不要で「どのアドレス/ポートを叩いたか」を診断できる）
	if len(candidateAddrs) > 0 {
		c.setStatusMessage(fmt.Sprintf("UDP candidates: %s", strings.Join(candidateAddrs, ", ")))
		if c.fileLog != nil {
			c.fileLog.Printf("UDP candidates: %s", strings.Join(candidateAddrs, ", "))
		}
	}

	// 候補アドレスを表示
	if c.isDebugEnabled() {
		for i, addr := range candidateAddrs {
			log.Printf("UDP candidate %d: %s", i+1, addr)
		}
	}

	// QUIC トランスポートを構築（認証は udpInfo.Key）。全候補に並行 Dial し、
	// 最初に確立したものを使う（localhost / LAN / グローバル STUN の選択）。
	// migration（ローミング）対応で候補は SetRoamingCandidates に渡す。
	if len(candidateAddrs) == 0 {
		return fmt.Errorf("no UDP candidate addresses")
	}

	ct, winnerAddr, failures := c.dialCandidatesParallel(candidateAddrs, udpInfo, clientID)
	if ct == nil {
		detail := strings.Join(failures, "; ")
		c.setStatusMessage("UDP: all candidates failed: " + detail)
		if c.fileLog != nil {
			c.fileLog.Printf("UDP: all candidates failed: %s", detail)
		}
		return fmt.Errorf("failed to connect to any candidate: %s", detail)
	}
	c.setStatusMessage(fmt.Sprintf("UDP connection established via %s", winnerAddr))

	ct.OnStateChange(func(oldState, newState transport.ConnectionState, message string) {
		c.setStatusMessage(fmt.Sprintf("[UDP] %s", message))
		c.addLogMessage(fmt.Sprintf("[UDP] state: %s", message))
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

	// ct は候補ループ内で既に Start 済み。ローミング候補を渡す。
	ct.SetRoamingCandidates(candidateAddrs)

	// 全候補失敗時の候補リフレッシャーを登録する。
	// reconnect がすべて失敗した場合のみ呼ばれる（通常 reconnect にはコスト増なし）。
	udpInfoCopy := udpInfo // クロージャにコピーを渡す
	ct.SetCandidatesRefresher(func() []string {
		var clientSTUN []string
		if len(udpInfoCopy.STUNAddrs) > 0 {
			clientSTUN = c.getClientSTUNAddrs()
		}
		fresh := buildUDPCandidateAddrs(udpInfoCopy, clientSTUN)
		if len(fresh) > 0 {
			c.addLogMessage(fmt.Sprintf("UDP: refreshed candidates: %s", strings.Join(fresh, ", ")))
		}
		return fresh
	})

	c.setTransport(ct)
	return nil
}

// retryUDPManager は QUIC 初回確立に失敗した場合に、UDS が生きている間
// バックグラウンドで再試行する。成功したら出力ポンプを起動して昇格する。
func (c *Client) retryUDPManager(udsAddr string, udpInfo *UDPInfo) {
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
			if err := c.startUDPManager(udsAddr, udpInfo); err != nil {
				if c.isDebugEnabled() {
					log.Printf("UDP: background retry failed: %v", err)
				}
				continue
			}
			c.setStatusMessage("[Tezzer] UDP connection established (background retry)")
			go c.handleUDPOutput()
			return
		}
	}
}

// getClientSTUNAddrs はクライアント側のSTUNアドレス候補を family ごとに取得する
// （NAT hole punching・接続候補生成のため。ipv4Only なら v4 のみ問い合わせる）。
func (c *Client) getClientSTUNAddrs() []string {
	var addrs []string
	if addr, err := c.getClientSTUNAddr("udp4"); err == nil {
		addrs = append(addrs, addr.String())
	}
	if !c.ipv4Only {
		if addr, err := c.getClientSTUNAddr("udp6"); err == nil {
			addrs = append(addrs, addr.String())
		}
	}
	return addrs
}

// getClientSTUNAddr はクライアント側のSTUNアドレスを指定 family（"udp4"/"udp6"）で取得する。
func (c *Client) getClientSTUNAddr(network string) (*net.UDPAddr, error) {
	// STUN A/Bサーバー（Google STUN使用）
	serverA := "stun.l.google.com:19302"
	serverB := "stun1.l.google.com:19302"

	stunClient := stun.NewClient(serverA)
	stunClient.Network = network

	// NAT判別と公開アドレス取得
	natType, mappedAddr, err := stunClient.DetectNATType(serverA, serverB)
	if err != nil {
		return nil, err
	}

	// NAT種別の文字列化とログ出力
	var natTypeStr string
	switch natType {
	case stun.NATTypeFullCone:
		natTypeStr = "Full Cone (or Port Restricted)"
	case stun.NATTypeSymmetric:
		natTypeStr = "Symmetric"
	default:
		natTypeStr = "Unknown"
	}

	c.setStatusMessage(fmt.Sprintf("Client NAT type (%s): %s", network, natTypeStr))

	return mappedAddr, nil
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

// dialCandidatesParallel は全候補に並行 Dial し、最初に確立した接続を返す。
// 高遅延リンク（RTT 数秒）でも 10s 以内に成功できるよう余裕を持たせる。
// タイムアウトは候補ごとではなく全体で 10s（並行なので直列より大幅に速い）。
func (c *Client) dialCandidatesParallel(candidateAddrs []string, udpInfo *UDPInfo, clientID uint16) (transport.ClientTransport, string, []string) {
	type dialResult struct {
		ct   transport.ClientTransport
		addr string
		fail string
	}

	ch := make(chan dialResult, len(candidateAddrs))
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)

	var wg sync.WaitGroup
	for _, addr := range candidateAddrs {
		addr := addr
		wg.Add(1)
		go func() {
			defer wg.Done()
			cand, err := qtransport.NewClient(udpInfo.Key, addr, clientID, c.sessionID)
			if err != nil {
				ch <- dialResult{addr: addr, fail: fmt.Sprintf("%s: new=%v", addr, err)}
				return
			}
			if c.agentSockPath != "" {
				if as, ok := cand.(interface{ SetAgentSockPath(string) }); ok {
					as.SetAgentSockPath(c.agentSockPath)
				}
			}
			// UDS 経由で既に描画済みの分は QUIC の再同期対象から外す
			// （attach 直後のバックログ二重転送の防止。Hello の LastOffset に反映される）。
			if seed := c.renderedSeq.Load(); seed > 0 {
				if sr, ok := cand.(interface{ SeedResyncOffset(uint64) }); ok {
					sr.SeedResyncOffset(seed)
				}
			}
			if err := cand.Start(dialCtx); err != nil {
				_ = cand.Close()
				if c.fileLog != nil {
					c.fileLog.Printf("UDP: candidate %s failed: %v", addr, err)
				}
				ch <- dialResult{addr: addr, fail: fmt.Sprintf("%s: %v", addr, err)}
				return
			}
			ch <- dialResult{ct: cand, addr: addr}
		}()
	}

	go func() {
		wg.Wait()
		close(ch)
		dialCancel()
	}()

	var winner transport.ClientTransport
	var winnerAddr string
	var failures []string
	for r := range ch {
		if r.fail != "" {
			failures = append(failures, r.fail)
			continue
		}
		if winner == nil {
			winner = r.ct
			winnerAddr = r.addr
			dialCancel() // 残りの Dial をキャンセル
		} else {
			_ = r.ct.Close() // 余分に成功した接続を閉じる
		}
	}
	return winner, winnerAddr, failures
}

// sendUDPClientInfo はクライアントのUDP情報をサーバーに送信
func (c *Client) sendUDPClientInfo(clientID uint16) error {
	msg := proto.UDPClientInfoMsg{
		Type:      "UDP_CLIENT_INFO",
		SessionID: c.sessionID,
		ClientID:  clientID,
	}
	data, err := proto.Encode(msg)
	if err != nil {
		return fmt.Errorf("failed to encode UDP client info: %w", err)
	}
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return netx.WriteFrame(c.conn, data)
}

// fetchServerMeta はサーバーのメタ情報を取得
func (c *Client) fetchServerMeta() error {
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

	if err := netx.WriteFrame(c.conn, data); err != nil {
		return fmt.Errorf("failed to write get server meta: %w", err)
	}

	// 応答を読む
	frameData, err := netx.ReadFrame(c.conn)
	if err != nil {
		return fmt.Errorf("failed to read server meta: %w", err)
	}

	respMsg, err := proto.Decode(frameData)
	if err != nil {
		return fmt.Errorf("failed to decode server meta: %w", err)
	}

	metaMsg, ok := respMsg.(*proto.ServerMetaMsg)
	if !ok {
		return fmt.Errorf("expected SERVER_META, got %T", respMsg)
	}

	c.metaMu.Lock()
	c.serverMeta = metaMsg
	c.metaMu.Unlock()

	return nil
}

// handleUDPOutput は QUIC 経由の出力を処理し stdout に書く。
func (c *Client) handleUDPOutput() {
	ct := c.transport()
	if ct == nil {
		return
	}
	var firstOutput sync.Once
	for {
		select {
		case <-c.done:
			return
		case chunk, ok := <-ct.Output():
			if !ok {
				return
			}
			firstOutput.Do(func() {
				c.addLogMessage(fmt.Sprintf("UDP: first server output received (%d bytes)", len(chunk.Data)))
			})
			// チャネルに溜まっている後続フレームを non-blocking でまとめ、
			// stdout への Write を 1 回にする（バースト出力時の syscall 削減）。
			// まとめすぎて書き出しが遅れないよう上限を設ける。
			const maxCoalesce = 256 * 1024
			chunks := []transport.OutputChunk{chunk}
			total := len(chunk.Data)
		drain:
			for total < maxCoalesce {
				select {
				case more, ok := <-ct.Output():
					if !ok {
						break drain
					}
					chunks = append(chunks, more)
					total += len(more.Data)
				default:
					break drain
				}
			}
			if c.isDebugEnabled() {
				log.Printf("UDP: [Client] received output (%d bytes), writing to stdout\n", total)
			}
			c.renderOutput(chunks, true)
		}
	}
}

// sendInputBytes は入力バイト列をサーバーに送信する
func (c *Client) sendInputBytes(data []byte) error {
	// -peek: 入力は一切送らない（誤爆防止）。打鍵のたびに騒がないよう初回のみ通知
	if c.readOnly {
		if !c.roWarned {
			c.roWarned = true
			c.setStatusMessage("read-only attach (-peek): input is not sent")
		}
		return nil
	}
	if ct := c.transport(); ct != nil {
		return ct.SendInput(data)
	}

	// TCP経由で送信
	inputMsg := proto.InputMsg{
		Type:      "INPUT",
		SessionID: c.sessionID,
		Data:      data,
	}
	inputData, err := proto.Encode(inputMsg)
	if err != nil {
		return fmt.Errorf("encode input error: %w", err)
	}
	c.connMu.Lock()
	err = netx.WriteFrame(c.conn, inputData)
	c.connMu.Unlock()
	if err != nil {
		return fmt.Errorf("write input error: %w", err)
	}
	return nil
}

// flushEscBatch はESC batchingバッファをフラッシュして送信する
func (c *Client) flushEscBatch(reason string) {
	if !c.escBatchActive || len(c.escBatchBuf) == 0 {
		return
	}

	delay := time.Since(c.escBatchStart)

	// デバッグログ出力
	if c.isDebugEnabled() {
		log.Printf("INPUT: esc-batch flush reason=%s len=%d delay_ms=%.2f hex=%x",
			reason, len(c.escBatchBuf), float64(delay.Microseconds())/1000.0, c.escBatchBuf)
	}

	// 送信（sendInputBytes は同期的にエンコードして返るので元バッファをそのまま渡す）
	if err := c.sendInputBytes(c.escBatchBuf); err != nil {
		log.Printf("ESC batch send error: %v", err)
	}

	// 状態リセット
	c.escBatchActive = false
	c.escBatchBuf = nil
}

// utf8IncompleteTrail は data 末尾の不完全な UTF-8 シーケンスのバイト数を返す。
// 末尾が完全な UTF-8 文字で終わっている場合は 0 を返す。
func utf8IncompleteTrail(data []byte) int {
	n := len(data)
	if n == 0 {
		return 0
	}
	// 末尾から最大3バイトさかのぼってマルチバイト先頭バイトを探す
	// UTF-8 の先頭バイトは 0xC0 以上（continuation byte は 0x80-0xBF）
	limit := 3
	if limit > n {
		limit = n
	}
	for i := 1; i <= limit; i++ {
		b := data[n-i]
		if b < 0x80 {
			// ASCII: ここで完結しているので不完全なし
			return 0
		}
		if b >= 0xC0 {
			// マルチバイト先頭バイト発見
			// 期待される全体バイト数を求める
			var expected int
			if b < 0xE0 {
				expected = 2
			} else if b < 0xF0 {
				expected = 3
			} else {
				expected = 4
			}
			if i < expected {
				// 不完全: i バイトしかないが expected バイト必要
				return i
			}
			// 完全なシーケンス
			return 0
		}
		// 0x80-0xBF: continuation byte, さらにさかのぼる
	}
	// 4バイト以上 continuation byte が続く場合は壊れているので 0
	return 0
}

// flushInputBatch は通常入力batchingバッファをフラッシュして送信する
func (c *Client) flushInputBatch(reason string) {
	if len(c.inputBatchBuf) == 0 {
		return
	}

	sendBuf := c.inputBatchBuf

	// UTF-8 境界を意識して分割する（マルチバイト文字がパケット境界で分断されるのを防ぐ）
	var holdBack []byte
	trail := utf8IncompleteTrail(sendBuf)
	if trail > 0 {
		holdBack = make([]byte, trail)
		copy(holdBack, sendBuf[len(sendBuf)-trail:])
		sendBuf = sendBuf[:len(sendBuf)-trail]
	}

	delay := time.Since(c.inputBatchStart)
	bufLen := len(sendBuf)

	if bufLen > 0 {
		// デバッグログ出力
		if c.isDebugEnabled() {
			log.Printf("INPUT: batch flush reason=%s len=%d delay_ms=%.2f",
				reason, bufLen, float64(delay.Microseconds())/1000.0)
		}

		// 送信（sendInputBytes は同期的にエンコードして返るので元バッファをそのまま渡す）
		if err := c.sendInputBytes(sendBuf); err != nil {
			log.Printf("Input batch send error: %v", err)
		}
	}

	// 不完全な UTF-8 シーケンスがあれば次のバッチに持ち越す
	if len(holdBack) > 0 {
		c.inputBatchBuf = holdBack
		c.inputBatchStart = time.Now()
	} else {
		c.inputBatchBuf = nil
	}
}

// isCSIFinal は CSI シーケンスの終端バイトかどうかを判定する
// CSI の final byte は '~' または英字 (A-Z, a-z)
func isCSIFinal(b byte) bool {
	if b == '~' {
		return true
	}
	if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
		return true
	}
	return false
}

// shouldFlushEscBuf は ESC バッファを即座にフラッシュすべきかを判定する
// - ESC O A/B/C/D (application cursor keys): 即 flush
// - ESC [ A/B/C/D (CSI cursor keys): 即 flush
// - その他の CSI シーケンス: final byte まで待つ
func shouldFlushEscBuf(buf []byte) bool {
	bufLen := len(buf)

	// 最大バイト数に達した場合は強制 flush
	if bufLen >= 32 {
		return true
	}

	if bufLen < 3 {
		return false
	}

	if buf[0] != 0x1b {
		return false
	}

	// ESC O A/B/C/D (application cursor keys)
	if buf[1] == 'O' {
		switch buf[2] {
		case 'A', 'B', 'C', 'D':
			return true
		default:
			return false
		}
	}

	// ESC [ で始まる CSI シーケンス
	if buf[1] == '[' {
		// ESC [ A/B/C/D (cursor keys) は即 flush
		if bufLen == 3 {
			switch buf[2] {
			case 'A', 'B', 'C', 'D':
				return true
			}
		}
		// それ以外は final byte で判定
		if isCSIFinal(buf[bufLen-1]) {
			return true
		}
		return false
	}

	return false
}

// handleStdin reads from stdin and sends to server
func (c *Client) handleStdin() {
	// フラッシュタイマー（バッチ開始時のみアーム、アイドル時は停止）
	flushTimer := time.NewTimer(0)
	if !flushTimer.Stop() {
		<-flushTimer.C
	}
	defer flushTimer.Stop()

	resetFlushTimer := func(d time.Duration) {
		if !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
		flushTimer.Reset(d)
	}
	// バッチ状態に基づいてタイマーを更新する（バッチがなければ停止）
	recheckFlushTimer := func() {
		var d time.Duration
		if c.escBatchActive {
			d = 6*time.Millisecond - time.Since(c.escBatchStart)
		} else if len(c.inputBatchBuf) > 0 {
			d = 2*time.Millisecond - time.Since(c.inputBatchStart)
		} else {
			if !flushTimer.Stop() {
				select {
				case <-flushTimer.C:
				default:
				}
			}
			return
		}
		if d < 0 {
			d = 0
		}
		resetFlushTimer(d)
	}

	stdinCh := make(chan struct {
		data []byte
		n    int
		err  error
	}, 1)

	// stdin読み取りをgoroutineで実行
	go func() {
		for {
			readBuf := make([]byte, 4096)
			n, err := os.Stdin.Read(readBuf)
			data := make([]byte, n)
			copy(data, readBuf[:n])
			stdinCh <- struct {
				data []byte
				n    int
				err  error
			}{data: data, n: n, err: err}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-flushTimer.C:
			if c.escBatchActive {
				c.flushEscBatch("timeout")
			}
			if len(c.inputBatchBuf) > 0 {
				c.flushInputBatch("timeout")
			}
			recheckFlushTimer()

		case result := <-stdinCh:
			n := result.n
			err := result.err
			data := result.data

			if err != nil {
				if err != io.EOF {
					log.Printf("stdin read error: %v\n", err)
				}
				c.doneOnce.Do(func() { close(c.done) })
				return
			}

			if n > 0 {
				// 1バイトずつ処理
				for i := 0; i < n; i++ {
					b := data[i]

					// エスケープキー（Ctrl-^ など）の処理
					if c.escapePressed {
						c.escapePressed = false
						if b == '.' {
							// エスケープキー + . でデタッチ（正常終了）
							fmt.Fprintf(os.Stderr, "\033[?25h")
							select {
							case c.errCh <- errDetached:
							default:
							}
							c.doneOnce.Do(func() { close(c.done) })
							time.Sleep(10 * time.Millisecond)
							c.conn.Close()
							fmt.Fprintf(os.Stderr, "\r\nDetached from session.\r\n")
							return
						}
						if b == 'i' {
							c.showStatus()
							continue
						}
						if b == 'h' {
							c.showHelp()
							continue
						}
						if b == 's' {
							go c.writeStatsFile()
							continue
						}
						if b == 'd' {
							c.toggleDebug()
							continue
						}
						if b == 'q' {
							// エスケープキー + q でセッション終了（-peek では無効）
							if c.readOnly {
								c.setStatusMessage("read-only attach (-peek): kill is disabled")
								continue
							}
							c.killSession()
							return
						}
						if b == 'r' {
							// エスケープキー + r でスクロール領域リセット＋画面リフレッシュ
							c.resetScrollRegion()
							continue
						}
						if b == 'f' {
							// エスケープキー + f で resize trick による強制再描画
							// （-peek では無効: 自分の端末サイズでリモート PTY を触ってしまうため）
							if c.readOnly {
								c.setStatusMessage("read-only attach (-peek): server redraw is disabled")
								continue
							}
							c.requestServerRedraw()
							continue
						}
						if b == c.escapeByte {
							// エスケープキー + エスケープキー でエスケープキーそのものを送信
							if err := c.sendInputBytes([]byte{b}); err != nil {
								log.Printf("send input error: %v", err)
								c.doneOnce.Do(func() { close(c.done) })
								return
							}
							continue
						}
						// その他のエスケープシーケンスは無視
						continue
					}

					if b == c.escapeByte {
						c.escapePressed = true
						continue
					}

					// ESC batching ロジック
					if b == 0x1b { // ESC
						// 既存のバッファがあれば先にフラッシュ
						if c.escBatchActive {
							c.flushEscBatch("new-esc")
						}
						// inputBatch が残っていれば先にフラッシュ（送信順序保証）
						// \e[201~（bracketed paste 終端）が content より先に届くのを防ぐ
						if len(c.inputBatchBuf) > 0 {
							c.flushInputBatch("pre-esc")
						}
						// 新しいESC batchingを開始
						c.escBatchActive = true
						c.escBatchBuf = []byte{0x1b}
						c.escBatchStart = time.Now()
						continue
					}

					if c.escBatchActive {
						// ESC batching中
						c.escBatchBuf = append(c.escBatchBuf, b)

						// 即座にフラッシュすべきか判定
						if shouldFlushEscBuf(c.escBatchBuf) {
							c.flushEscBatch("immediate")
						}
						continue
					}

					// 通常入力の一括処理: 次の ESC/escapeByte まで一括 append
					j := i + 1
					for j < n && data[j] != 0x1b && data[j] != c.escapeByte {
						j++
					}
					if len(c.inputBatchBuf) == 0 {
						c.inputBatchStart = time.Now()
					}
					c.inputBatchBuf = append(c.inputBatchBuf, data[i:j]...)
					if len(c.inputBatchBuf) >= 512 {
						c.flushInputBatch("size")
					}
					i = j - 1 // ループの i++ で j になる
				}
			}

			// 読み取り終了後、バッファにデータがあれば即座にフラッシュ
			// （ペースト全体を一度に送信するため）
			if len(c.inputBatchBuf) > 0 {
				c.flushInputBatch("read-end")
			}
			recheckFlushTimer()
		}
	}
}

// handleServer reads from server and writes to stdout
func (c *Client) handleServer() {
	for {
		select {
		case <-c.done:
			// クリーンな切断（Ctrl-^ . など）
			return
		default:
		}

		// 接続を取得（ロックして即座に解放）
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		frameData, err := netx.ReadFrame(conn)
		if err != nil {
			// doneが閉じている場合は、ユーザーが意図的に切断したのでメッセージ不要
			select {
			case <-c.done:
				return
			default:
			}

			// QUIC が有効な場合は制御チャネル（UDS）切断をログのみで続行
			if c.transport() != nil {
				if err != io.EOF {
					log.Printf("UDS control channel lost: %v", err)
				}
				c.setStatusMessage("[Tezzer] UDS control lost (QUIC continuing)")
				return
			}

			// UDP無効時はクライアント全体を異常終了
			select {
			case c.errCh <- fmt.Errorf("UDS read error: %w", err):
			default:
			}
			c.doneOnce.Do(func() { close(c.done) })
			return
		}

		msg, err := proto.Decode(frameData)
		if err != nil {
			log.Printf("decode error: %v", err)
			continue
		}

		switch m := msg.(type) {
		case *proto.OutputMsg:
			// 描画は renderOutput が renderedSeq で判定する（クロスパス重複排除）。
			// QUIC 接続中は通常サーバ側ゲーティングにより UDS 出力は届かないが、
			// 届いた場合（旧サーバ・ゲーティング切替の境界・QUIC 断中のフォール
			// バック）も一度だけ描画される。
			if c.transport() == nil {
				// UDS 単独経路のときだけ欠番を警告（QUIC 併用時は QUIC が信頼配送で埋める）
				lastSeq := c.lastSeq.Load()
				if lastSeq > 0 && m.Seq > lastSeq+1 {
					missing := m.Seq - lastSeq - 1
					c.setStatusMessage(fmt.Sprintf("UDS: %d messages lost (continuing)", missing))
				}
			}
			c.lastSeq.Store(m.Seq)

			// PTY出力をそのままstdoutに書く（OSC含む）
			c.renderOutput([]transport.OutputChunk{{Offset: m.Seq, Data: m.Data}}, false)

		case *proto.ErrorMsg:
			log.Printf("server error: %s: %s", m.Code, m.Message)

		case *proto.NoteMsg:
			// OUTPUT_DROPPEDの場合はステータスで通知
			if m.Kind == "OUTPUT_DROPPED" {
				// QUIC 接続時は UDS の OutCh 詰まり由来の OUTPUT_DROPPED は誤検知（出力は
				// QUIC で信頼配送される）。真の欠損は再接続時に QUIC の ctrlStatus で届くので
				// ここでは無視する。
				if c.transport() != nil {
					continue
				}
				msg := "Output was dropped (use Ctrl-^ r to redraw)"
				if m.Msg != "" {
					msg = m.Msg
				}
				c.setStatusMessage(msg)
			} else if m.Kind == "SESSION_CLOSED" {
				// セッション終了時の通知（UDS 経由）
				// QUIC 側でも同通知が来うるため、先に表示済みフラグを立てて重複を防ぐ。
				c.sessionClosedNotified.Store(true)
				if m.ExitCode != nil {
					c.noteSessionExitCode(*m.ExitCode)
				}
				fmt.Fprintf(os.Stderr, "\r\n[Tezzer] Session closed: %s\r\n", m.Msg)
				c.doneOnce.Do(func() { close(c.done) })
				return
			}

		case *proto.ServerMetaMsg:
			// サーバーメタ情報を受信
			c.metaMu.Lock()
			c.serverMeta = m
			c.metaMu.Unlock()

		default:
			// 未知のメッセージは無視
		}
	}
}

const (
	rttHistorySize    = 10
	rttSampleInterval = 3 * time.Second
)

// sampleRTTHistory は Ctrl-^ i のスパークライン表示用に RTT を定期サンプリングする。
// トランスポート未確立中（UDS のみ）はスキップする。
func (c *Client) sampleRTTHistory() {
	ticker := time.NewTicker(rttSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			ct := c.transport()
			if ct == nil {
				continue
			}
			rtt := ct.Stats().RTT
			c.rttHistoryMu.Lock()
			c.rttHistory = append(c.rttHistory, rtt)
			if len(c.rttHistory) > rttHistorySize {
				c.rttHistory = c.rttHistory[len(c.rttHistory)-rttHistorySize:]
			}
			c.rttHistoryMu.Unlock()
		}
	}
}

var sparklineBlocks = []rune("▁▂▃▄▅▆▇█")

// renderSparkline は RTT サンプル列をウィンドウ内 min/max 正規化した
// ブロック文字列に変換する（直近の遅延変動を一目で見るため）。
func renderSparkline(samples []float64) string {
	if len(samples) == 0 {
		return ""
	}
	lo, hi := samples[0], samples[0]
	for _, v := range samples {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	span := hi - lo
	out := make([]rune, len(samples))
	for i, v := range samples {
		if span == 0 {
			out[i] = sparklineBlocks[0]
			continue
		}
		idx := int((v - lo) / span * float64(len(sparklineBlocks)-1))
		out[i] = sparklineBlocks[idx]
	}
	return string(out)
}

// showStatus displays status information for 3 seconds
func (c *Client) showStatus() {
	sessID := c.sessionID
	if len(sessID) > 6 {
		sessID = sessID[:6]
	}
	transportInfo := "UDS"
	if ct := c.transport(); ct != nil {
		rtt := ct.Stats().RTT
		c.rttHistoryMu.Lock()
		spark := renderSparkline(c.rttHistory)
		c.rttHistoryMu.Unlock()
		transportInfo = fmt.Sprintf("%s RTT:%.0fms %s", ct.State(), rtt, spark)
	}
	outInfo := ""
	if t := c.lastOutputRecv.Load(); t != 0 {
		elapsed := time.Since(time.Unix(0, t)).Round(time.Second)
		outInfo = fmt.Sprintf(" | out:%s", elapsed)
	}
	fwdInfo := ""
	if len(c.forwards) > 0 {
		fwdInfo = fmt.Sprintf(" | fwd:%d/%d", c.fwdActive.Load(), len(c.forwards))
	}
	status := fmt.Sprintf("[Tezzer] sess:%s | %s%s%s", sessID, transportInfo, outInfo, fwdInfo)
	c.setStatusMessage(status)
}

func (c *Client) showHelp() {
	escKey := escapeKeyDisplay(c.escapeByte)
	help := fmt.Sprintf("[Tezzer] %s+ .Detach qQuit rRedraw fForce iStatus sStats dDebug hHelp", escKey)
	c.setStatusMessage(help)
}

// killSession はセッションを終了し、クライアントを終了する
func (c *Client) killSession() {
	fmt.Fprintf(os.Stderr, "\033[?25h")

	// kill 中フラグを先に立てる（QUIC sessionGone がエラー扱いされないように）
	c.killing.Store(true)

	killMsg := proto.KillSessionMsg{
		Type:      "KILL_SESSION",
		SessionID: c.sessionID,
	}
	killData, err := proto.Encode(killMsg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\r\nError encoding kill message: %v\r\n", err)
		c.forceExit()
		return
	}

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		fmt.Fprintf(os.Stderr, "\r\nSession %s: connection not available\r\n", c.sessionID)
		c.forceExit()
		return
	}

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := netx.WriteFrame(conn, killData); err != nil {
		fmt.Fprintf(os.Stderr, "\r\nFailed to send kill: %v\r\n", err)
		c.forceExit()
		return
	}

	// UDS 応答は待たない。QUIC の sessionGone が確認経路。
	fmt.Fprintf(os.Stderr, "\r\nSession %s killed.\r\n", c.sessionID)
	c.forceExit()
}

// forceExit は強制的にクライアントを終了する（killSession用ヘルパー）
func (c *Client) forceExit() {
	// 終了処理
	select {
	case c.errCh <- errDetached:
	default:
	}
	c.doneOnce.Do(func() { close(c.done) })

	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.connMu.Unlock()
}

// resetScrollRegion はスクロール領域をリセットし、画面をリフレッシュする
func (c *Client) resetScrollRegion() {
	// スクロール領域をデフォルト（全画面）にリセット
	// \e[r = DECSTBM（Set Top and Bottom Margins）パラメータなしでデフォルト
	fmt.Fprintf(os.Stderr, "\033[r")

	// 画面をクリアして再描画を促す
	// \e[2J = 画面全体をクリア、\e[H = カーソルをホームポジションへ
	fmt.Fprintf(os.Stderr, "\033[2J\033[H")

	c.setStatusMessage("Scroll region reset")
}

// requestServerRedraw はサーバー側 PTY に resize trick で再描画を促す。
// UDP resyncing 後（OutputRingBuffer の replay が発生したケース）に呼ぶ。
// TCP reattach は session.RequestRedraw がサーバー側で直接処理する。
func (c *Client) requestServerRedraw() {
	go func() {
		time.Sleep(300 * time.Millisecond)
		w, h, err := term.GetSize(c.termFd)
		if err != nil || w <= 1 {
			return
		}
		c.sendResize(w-1, h)
		time.Sleep(50 * time.Millisecond)
		// 待機中に本物の SIGWINCH resize が入っている可能性があるため、
		// 縮小前のサイズを使い回さず、端末の現在サイズを取り直して復元する。
		w, h, err = term.GetSize(c.termFd)
		if err != nil {
			return
		}
		c.sendResize(w, h)
	}()
}

// setStatusMessage はステータスメッセージをログに追加し、3秒間表示する
func (c *Client) setStatusMessage(msg string) {
	c.addLogMessage(msg)
	if c.statusMgr == nil {
		// -N（転送専用）ではステータス行がないので通常のログに出す
		log.Print(msg)
		return
	}
	c.statusMgr.Set(msg)
}

// addLogMessage は通知メッセージをログバッファに追加し、ログファイルにも書き出す
func (c *Client) addLogMessage(msg string) {
	c.msgLogMu.Lock()
	defer c.msgLogMu.Unlock()

	entry := logEntry{
		timestamp: time.Now(),
		message:   msg,
	}

	c.msgLog = append(c.msgLog, entry)

	// 最大件数を超えたら古いものを削除
	if len(c.msgLog) > maxLogEntries {
		c.msgLog = c.msgLog[len(c.msgLog)-maxLogEntries:]
	}

	// ファイルにも書き出す（minimal ログ）
	c.fileLog.Print(msg)
}

// getLogMessages はログメッセージのコピーを返す
func (c *Client) getLogMessages() []logEntry {
	c.msgLogMu.RLock()
	defer c.msgLogMu.RUnlock()

	// コピーを返す
	result := make([]logEntry, len(c.msgLog))
	copy(result, c.msgLog)
	return result
}

// renderOutput は出力チャンク列を重複排除しつつ stdout へ描画する。
// UDS（OutputMsg.Seq）と QUIC（出力 offset）は同じ番号空間（チャンクごとに +1）なので、
// 描画済み最大オフセット（renderedSeq）1 つでクロスパスの一度だけ描画を保証できる。
//
// fromQUIC = true（信頼・順序配送）は frontier からのジャンプを許す（リングバッファ
// evict 由来の正当な欠落。別途 redraw を促すステータス通知が届く）。
// fromQUIC = false（UDS。サーバ側 OutCh あふれで欠落しうる）は、QUIC 併用時は
// 厳密連続（frontier+1）のみ描画し、欠落を跨いだチャンクは QUIC 側の信頼配送に
// 任せて捨てる。QUIC 未確立（UDS 単独）のときは従来どおり欠落を跨いで描画する。
func (c *Client) renderOutput(chunks []transport.OutputChunk, fromQUIC bool) {
	c.renderMu.Lock()
	defer c.renderMu.Unlock()
	frontier := c.renderedSeq.Load()
	var buf []byte
	for _, ch := range chunks {
		switch {
		case ch.Offset <= frontier:
			// 描画済み（他経路が先行）
		case ch.Offset == frontier+1 || fromQUIC || c.transport() == nil:
			buf = append(buf, ch.Data...)
			frontier = ch.Offset
		default:
			// UDS 経路の欠落を跨いだチャンク: QUIC が同じ内容を信頼配送するので捨てる
		}
	}
	if len(buf) == 0 {
		return
	}
	c.renderedSeq.Store(frontier)
	c.writeOutput(buf)
}

func (c *Client) writeOutput(data []byte) {
	c.lastOutputRecv.Store(time.Now().UnixNano())
	if c.noPTY {
		// -N: 出力は表示しない。ただし読み捨ては必要（読まないとサーバ側の
		// 出力 Write が詰まり、他クライアントや PTY reader に波及する）。
		return
	}
	os.Stdout.Write(data)
}

// isDebugEnabled はデバッグ出力が有効かどうかを返す
func (c *Client) isDebugEnabled() bool {
	v := c.debugEnabled.Load()
	if v == nil {
		return false
	}
	return v.(bool)
}

// toggleDebug はデバッグ出力のON/OFFをトグルする
func (c *Client) toggleDebug() {
	oldValue := c.isDebugEnabled()
	newValue := !oldValue
	c.debugEnabled.Store(newValue)

	// debug ON/OFF に応じて log.SetOutput をファイルにティーするか切り替える
	stderrWriter := &termui.CRLFWriter{W: os.Stderr}
	if newValue && c.logFile != nil {
		log.SetOutput(io.MultiWriter(stderrWriter, c.logFile))
	} else {
		log.SetOutput(stderrWriter)
	}

	// ステータスメッセージで通知
	status := "OFF"
	if newValue {
		status = "ON"
	}
	c.setStatusMessage(fmt.Sprintf("[Tezzer] Debug output: %s", status))
}

// sendResize sends a resize message to the server
func (c *Client) sendResize(width, height int) error {
	// -peek: リモート PTY のサイズを変えない（他クライアントと本体の表示を乱さない）
	if c.readOnly {
		return nil
	}
	c.width = width
	c.height = height

	// QUIC トランスポートがある場合はそちらで送信
	if ct := c.transport(); ct != nil {
		if err := ct.SendResize(width, height); err != nil {
			// スリープ復帰直後など、再接続完了前に resize が飛ぶと一時的に失敗しうる
			// （requestServerRedraw が再接続後に現在サイズを送り直すため実害はない）。
			// 生の log.Printf は raw mode の端末内容に直接混ざるため、他の一時的な
			// ネットワーク断メッセージと同様 setStatusMessage 経由にする。
			c.setStatusMessage(fmt.Sprintf("QUIC send resize error: %v", err))
		}
		return nil
	}

	// TCP経由で送信
	resizeMsg := proto.ResizeMsg{
		Type:      "RESIZE",
		SessionID: c.sessionID,
		Cols:      width,
		Rows:      height,
	}
	resizeData, err := proto.Encode(resizeMsg)
	if err != nil {
		return fmt.Errorf("encode resize error: %w", err)
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	if err := netx.WriteFrame(c.conn, resizeData); err != nil {
		return fmt.Errorf("write resize error: %w", err)
	}
	return nil
}

// restoreTerminalFlags はターミナルフラグを復元する（ONLCR等）
// プラットフォーム固有の実装は terminal_*.go に定義されている

// 管理コマンド実装

// formatAgo は経過時間を "3s", "5m", "2h", "3d" の形式で返す。
func formatAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// fetchSessions はサーバから SESSIONS_LIST を取得する
// （-list / -resume / -name の共通処理）。
func fetchSessions(addr string) ([]proto.SessionInfo, error) {
	conn, err := connectToServer(addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	resp, err := roundTrip[proto.SessionsListMsg](conn, proto.ListSessionsMsg{Type: "LIST_SESSIONS"})
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

func listSessions(addr string, jsonOut bool) error {
	sessions, err := fetchSessions(addr)
	if err != nil {
		return err
	}

	if jsonOut {
		out, err := formatSessionsListJSON(sessions, time.Local)
		if err != nil {
			return fmt.Errorf("json encode error: %w", err)
		}
		fmt.Print(out)
		return nil
	}

	fmt.Print(formatSessionsList(sessions, time.Local))
	return nil
}

// findSessionIDByName は名前が一致するアクティブなセッションの ID を返す。
// 見つからなければ空文字（エラーではない）。PTY 終了済みセッションは
// 名前を保持しない扱い（サーバ側の一意性チェックと同じ規則）。
func findSessionIDByName(addr, name string) (string, error) {
	sessions, err := fetchSessions(addr)
	if err != nil {
		return "", err
	}
	for i := range sessions {
		s := &sessions[i]
		if s.Name == name && !s.PTYClosed {
			return s.SessionID, nil
		}
	}
	return "", nil
}

// formatSessionsList は -list の表示文字列を組み立てる純粋関数。
// 時刻整形のロケーションを引数に取り、テストで再現可能にしている（本番は time.Local）。
func formatSessionsList(sessions []proto.SessionInfo, loc *time.Location) string {
	if len(sessions) == 0 {
		return "No active sessions\n"
	}

	// セッションIDの最大長を計算（最小22文字）
	maxIDLen := 22
	// NAME 列の幅（最小4文字＝ヘッダー幅、無名は "-" 表示）
	maxNameLen := 4
	for _, s := range sessions {
		if len(s.SessionID) > maxIDLen {
			maxIDLen = len(s.SessionID)
		}
		if len(s.Name) > maxNameLen {
			maxNameLen = len(s.Name)
		}
	}

	var b strings.Builder
	// ヘッダー出力
	fmt.Fprintf(&b, "%-*s %-*s %-15s %4s %4s %8s %-6s %-19s %-11s %s\r\n", maxIDLen, "SESSION ID", maxNameLen, "NAME", "COMMAND", "ROWS", "COLS", "CLIENTS", "STATUS", "DETACHED", "LAST ACTIVE", "CREATED")
	for _, s := range sessions {
		createdTime := time.Unix(s.CreatedAt, 0).In(loc)
		status := "active"
		if s.PTYClosed {
			status = "closed"
		}
		detached := "-"
		if s.LastDetachedAt != 0 {
			detached = time.Unix(s.LastDetachedAt, 0).In(loc).Format("2006-01-02 15:04:05")
		}
		// 全クライアントの LastSeen の最大値をセッションの最終活動時刻とする
		lastActive := "-"
		var maxLastSeen int64
		for _, c := range s.Clients {
			if c.LastSeen > maxLastSeen {
				maxLastSeen = c.LastSeen
			}
		}
		if maxLastSeen > 0 {
			lastActive = formatAgo(time.Since(time.Unix(maxLastSeen, 0)))
		}
		sessName := s.Name
		if sessName == "" {
			sessName = "-"
		}
		fmt.Fprintf(&b, "%-*s %-*s %-15s %4d %4d %8d %-6s %-19s %-11s %s\r\n",
			maxIDLen, s.SessionID, maxNameLen, sessName, s.Cmd, s.Rows, s.Cols, s.ClientCount, status, detached, lastActive, createdTime.Format("2006-01-02 15:04:05"))

		// クライアント接続情報を表示（インデント付き）
		for _, c := range s.Clients {
			clientInfo := fmt.Sprintf("  - %s (%s)", c.Protocol, c.ID)
			if c.RemoteAddr != "" {
				clientInfo += fmt.Sprintf(" from %s", c.RemoteAddr)
			}
			if c.QUICRemoteAddr != "" {
				clientInfo += fmt.Sprintf(" quic=%s", c.QUICRemoteAddr)
			}
			if c.UDPClientID != 0 {
				clientInfo += fmt.Sprintf(" [UDP ClientID=%d", c.UDPClientID)
				if len(c.UDPAddresses) > 0 {
					clientInfo += fmt.Sprintf(" Addrs=%v", c.UDPAddresses)
				}
				clientInfo += "]"
			}
			if c.LastSeen > 0 {
				clientInfo += fmt.Sprintf(" last=%s", formatAgo(time.Since(time.Unix(c.LastSeen, 0))))
			}
			fmt.Fprintf(&b, "%s\r\n", clientInfo)
		}
	}
	return b.String()
}

func showSessionInfo(addr, sessionID string, jsonOut bool) error {
	conn, err := connectToServer(addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := roundTrip[proto.SessionInfoMsg](conn, proto.GetSessionInfoMsg{
		Type:      "GET_SESSION_INFO",
		SessionID: sessionID,
	})
	if err != nil {
		return err
	}
	s := resp.Session

	if jsonOut {
		out, err := formatSessionInfoJSON(s, time.Local)
		if err != nil {
			return fmt.Errorf("json encode error: %w", err)
		}
		fmt.Print(out)
		return nil
	}

	createdTime := time.Unix(s.CreatedAt, 0)
	fmt.Printf("Session ID: %s\n", s.SessionID)
	if s.Name != "" {
		fmt.Printf("Name:       %s\n", s.Name)
	}
	fmt.Printf("Command:    %s\n", s.Cmd)
	fmt.Printf("Size:       %d cols x %d rows\n", s.Cols, s.Rows)
	fmt.Printf("Clients:    %d\n", s.ClientCount)

	// PTY状態を表示
	if s.PTYClosed {
		fmt.Printf("Status:     Closed (PTY terminated)\n")
	} else {
		fmt.Printf("Status:     Active\n")
	}

	// セッション単位の freshness（attach していないクライアントからも見える活動指標）
	if s.LastOutputAt > 0 {
		t := time.Unix(s.LastOutputAt, 0)
		fmt.Printf("Last output: %s (%s ago)\n",
			t.Format("2006-01-02 15:04:05"), time.Since(t).Truncate(time.Second))
	}
	if s.LastInputAt > 0 {
		t := time.Unix(s.LastInputAt, 0)
		fmt.Printf("Last input:  %s (%s ago)\n",
			t.Format("2006-01-02 15:04:05"), time.Since(t).Truncate(time.Second))
	}

	// UDP接続情報を表示
	if s.UDPEnabled {
		fmt.Printf("UDP:        Enabled (port %d)\n", s.UDPPort)
	} else {
		fmt.Printf("UDP:        Disabled\n")
	}

	// OutputRingBuffer 統計を表示
	if s.OutputChunks > 0 || s.OutputBufferBytes > 0 {
		fmt.Printf("\nOutput Buffer:\n")
		fmt.Printf("  Chunks:      %d\n", s.OutputChunks)
		fmt.Printf("  Total bytes: %d\n", s.OutputBufferBytes)
		if s.OutputColdSegments > 0 {
			fmt.Printf("  Cold:        %d segments, %d bytes compressed (%d bytes raw)\n",
				s.OutputColdSegments, s.OutputColdBytes, s.OutputColdRawBytes)
		}
		if s.OldestChunkTime > 0 {
			oldestTime := time.Unix(s.OldestChunkTime, 0)
			age := time.Since(oldestTime).Truncate(time.Second)
			fmt.Printf("  Oldest:      %s (%s ago)\n", oldestTime.Format("2006-01-02 15:04:05"), age)
		}
	}

	// クライアント接続情報を表示
	if len(s.Clients) > 0 {
		fmt.Printf("\nConnected clients:\n")
		for _, c := range s.Clients {
			clientInfo := fmt.Sprintf("  - %s (%s)", c.Protocol, c.ID)
			if c.RemoteAddr != "" {
				clientInfo += fmt.Sprintf(" from %s", c.RemoteAddr)
			}
			if c.QUICRemoteAddr != "" {
				clientInfo += fmt.Sprintf(" quic=%s", c.QUICRemoteAddr)
			}
			// UDP接続情報を表示
			if c.UDPClientID != 0 {
				clientInfo += fmt.Sprintf(" [UDP ClientID=%d", c.UDPClientID)
				if len(c.UDPAddresses) > 0 {
					clientInfo += fmt.Sprintf(" Addrs=%v", c.UDPAddresses)
				}
				clientInfo += "]"
			}
			fmt.Printf("%s\n", clientInfo)
			// 送信統計を表示
			if c.SendBufferBytes > 0 {
				fmt.Printf("      Output sent: %d bytes\n", c.SendBufferBytes)
			}
			// backpressure 指標（出力 Write の詰まり）
			if c.SlowOutputWrites > 0 || c.MaxOutputWriteMs > 0 || c.OutputStallEpisodes > 0 {
				line := fmt.Sprintf("      Output backpressure: %d slow writes, max %d ms",
					c.SlowOutputWrites, c.MaxOutputWriteMs)
				if c.OutputStallEpisodes > 0 {
					line += fmt.Sprintf(", %d stalls", c.OutputStallEpisodes)
				}
				if c.OutputStallMs > 0 {
					line += fmt.Sprintf(" [STALLED NOW: %ds]", c.OutputStallMs/1000)
				}
				fmt.Println(line)
			}
			// TCP ポートフォワード統計
			if c.ForwardsOpened > 0 {
				fmt.Printf("      Forwards: %d active / %d total, %d bytes out / %d bytes in\n",
					c.ForwardsActive, c.ForwardsOpened, c.ForwardBytesToTarget, c.ForwardBytesFromTarget)
			}
			// LastSeen を表示
			if c.LastSeen > 0 {
				lastSeenTime := time.Unix(c.LastSeen, 0)
				ago := time.Since(lastSeenTime).Truncate(time.Second)
				fmt.Printf("      Last output: %s (%s ago)\n",
					lastSeenTime.Format("2006-01-02 15:04:05"), ago)
			}
		}
	}

	fmt.Printf("\nCreated:    %s\n", createdTime.Format("2006-01-02 15:04:05"))
	return nil
}

// waitSession はセッションのコマンド終了を待ち、その exit code を返す（-wait）。
// サーバは終了まで応答（NOTE: SESSION_CLOSED）を保留するため、roundTrip の
// ReadFrame がセッション終了までブロックする。exit code が不明な場合
// （セッションが kill された等）は attach 時の慣行に合わせて 0 を返す。
func waitSession(addr, sessionID string) (int, error) {
	conn, err := connectToServer(addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	note, err := roundTrip[proto.NoteMsg](conn, proto.WaitSessionMsg{
		Type:      "WAIT_SESSION",
		SessionID: sessionID,
	})
	if err != nil {
		return 0, err
	}
	if note.Kind != "SESSION_CLOSED" {
		return 0, fmt.Errorf("unexpected notification kind %q", note.Kind)
	}
	if note.ExitCode != nil {
		return *note.ExitCode, nil
	}
	return 0, nil
}

func killSession(addr, sessionID string) error {
	conn, err := connectToServer(addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	killedMsg, err := roundTrip[proto.SessionKilledMsg](conn, proto.KillSessionMsg{
		Type:      "KILL_SESSION",
		SessionID: sessionID,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Session %s killed\n", killedMsg.SessionID)
	return nil
}

// getLatestSessionID は最新のセッションIDを取得
func getLatestSessionID(addr string) (string, error) {
	sessions, err := fetchSessions(addr)
	if err != nil {
		return "", err
	}

	// 最新のアクティブなセッション（PTY終了済みを除く、CreatedAtが最も新しいもの）を選択
	var latestSession *proto.SessionInfo
	for i := range sessions {
		s := &sessions[i]
		// PTY終了済みセッションはスキップ
		if s.PTYClosed {
			continue
		}
		if latestSession == nil || s.CreatedAt > latestSession.CreatedAt {
			latestSession = s
		}
	}

	if latestSession == nil {
		return "", nil // アクティブなセッションがない
	}

	return latestSession.SessionID, nil
}

// formatBytes はバイト数を人間が読みやすい形式に変換する
func formatBytes(b uint64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// dialAndHandshake はサーバーに接続し HELLO/WELCOME ハンドシェイクを行う
func dialAndHandshake(addr string, cols, rows int) (net.Conn, error) {
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return nil, err
	}

	helloData, err := proto.Encode(proto.HelloMsg{
		Type:       "HELLO",
		V:          1,
		ClientName: "tezzer",
		Cols:       cols,
		Rows:       rows,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to encode hello: %w", err)
	}
	if err := netx.WriteFrame(conn, helloData); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to write hello: %w", err)
	}

	frameData, err := netx.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read welcome: %w", err)
	}
	msg, err := proto.Decode(frameData)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to decode welcome: %w", err)
	}
	welcome, ok := msg.(*proto.WelcomeMsg)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("expected WELCOME, got %T", msg)
	}
	log.Printf("connected to %s", welcome.ServerName)

	return conn, nil
}

// connectToServer は管理コマンド用のサーバー接続（端末サイズは不問）
func connectToServer(addr string) (net.Conn, error) {
	return dialAndHandshake(addr, 80, 24)
}
