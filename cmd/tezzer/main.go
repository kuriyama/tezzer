package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/termui"
	"github.com/kuriyama/tezzer/internal/version"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

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

	exitCode, err := run(connectAddr, *sessionID, *cmd, *name, escapeByte, *ipv4Only, forwards, *noPTY, *agentForward, *peek, agentSockPath)
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

// run はセッションへ接続して端末セッション（または -N の転送専用 attach）を実行する。
// noPTY（-N）のときは端末を触らず（raw mode・stdin・出力表示なし）、転送だけを行う。
// run はセッションへ接続し、終了コード（セッションプロセスのもの。不明・detach は 0）
// とエラーを返す。
func run(addr, sessionID, cmd, name string, escapeByte byte, ipv4Only bool, forwards []forwardSpec, noPTY, agentForward, readOnly bool, agentSockPath string) (int, error) {
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
	var quicInfo *QUICInfo // QUIC 接続情報（nil の場合は QUIC 無効）

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

		quicInfo = quicInfoFromSessionCreated(createdMsg)

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

		quicInfo = quicInfoFromSessionCreated(attachedMsg)
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

	// QUIC トランスポートを起動（QUIC 情報がある場合のみ）
	// STUN ルックアップを含むため数百ms かかることがある。
	// handleServer/handleStdin は既に起動済みで UDS で動作している。
	if quicInfo != nil {
		// QUIC セットアップは Dial が最大 10s かかりうるためバックグラウンドで行い、
		// メインのイベントループや UDS 経路を妨げない。
		go func() {
			if err := client.startQUICTransport(addr, quicInfo); err != nil {
				// 初回確立に失敗しても UDS-only で居座らせない（それは「ただの SSH」）。
				// UDS が生きている間、QUIC 確立を再試行する（高遅延でハンドシェイクが
				// 間に合わなかった等、後から繋がりうる）。
				client.setStatusMessage(fmt.Sprintf("QUIC setup failed, retrying in background: %v", err))
				client.retryQUICTransport(addr, quicInfo)
				return
			}
			client.setStatusMessage(fmt.Sprintf("QUIC enabled: server port %d", quicInfo.Port))
			go client.handleQUICOutput()
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
