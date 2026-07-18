package main

// admin.go: 管理コマンド（-list / -info / -wait / -kill）の実装と表示整形。
// サーバとの往復は rpc.go の roundTrip / connectToServer を使う。

import (
	"fmt"
	"github.com/kuriyama/tezzer/internal/proto"
	"strings"
	"time"
)

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
				clientInfo += fmt.Sprintf(" [QUIC ClientID=%d", c.UDPClientID)
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

	// QUIC 接続情報を表示（wire フィールド名は互換のため udp_* のまま）
	if s.UDPEnabled {
		fmt.Printf("QUIC:       Enabled (UDP port %d)\n", s.UDPPort)
	} else {
		fmt.Printf("QUIC:       Disabled\n")
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
			// QUIC 接続情報を表示
			if c.UDPClientID != 0 {
				clientInfo += fmt.Sprintf(" [QUIC ClientID=%d", c.UDPClientID)
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
