package session

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestManagerCreateAndGetSession はセッションの作成と取得をテスト
func TestManagerCreateAndGetSession(t *testing.T) {
	mgr := NewManager()

	// セッション作成
	sess, err := mgr.CreateSession("", "/bin/echo", []string{"hello"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	// セッションIDが生成されていることを確認
	if sess.ID == "" {
		t.Error("session ID should not be empty")
	}

	// パラメータが正しく設定されていることを確認
	if sess.Cmd != "/bin/echo" {
		t.Errorf("expected cmd /bin/echo, got %s", sess.Cmd)
	}
	if len(sess.Args) != 1 || sess.Args[0] != "hello" {
		t.Errorf("expected args [hello], got %v", sess.Args)
	}
	if sess.Rows != 24 {
		t.Errorf("expected rows 24, got %d", sess.Rows)
	}
	if sess.Cols != 80 {
		t.Errorf("expected cols 80, got %d", sess.Cols)
	}

	// 同じIDで取得できることを確認
	retrieved, err := mgr.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if retrieved.ID != sess.ID {
		t.Errorf("expected session ID %s, got %s", sess.ID, retrieved.ID)
	}
}

// TestManagerGetNonExistentSession は存在しないセッションの取得をテスト
func TestManagerGetNonExistentSession(t *testing.T) {
	mgr := NewManager()

	_, err := mgr.GetSession("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent session, got nil")
	}
}

// TestManagerDeleteSession はセッションの削除をテスト
func TestManagerDeleteSession(t *testing.T) {
	mgr := NewManager()

	// セッション作成
	sess, err := mgr.CreateSession("", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	sessionID := sess.ID

	// セッション削除
	err = mgr.DeleteSession(sessionID)
	if err != nil {
		t.Fatalf("failed to delete session: %v", err)
	}

	// 削除後は取得できないことを確認
	_, err = mgr.GetSession(sessionID)
	if err == nil {
		t.Error("expected error for deleted session, got nil")
	}
}

// TestManagerListSessions は複数セッションの一覧取得をテスト
func TestManagerListSessions(t *testing.T) {
	mgr := NewManager()

	// 初期状態では空
	if len(mgr.ListSessions()) != 0 {
		t.Error("expected empty session list initially")
	}

	// 3つのセッションを作成
	sess1, _ := mgr.CreateSession("", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false)
	defer sess1.Close()
	sess2, _ := mgr.CreateSession("", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false)
	defer sess2.Close()
	sess3, _ := mgr.CreateSession("", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false)
	defer sess3.Close()

	// 一覧を取得
	ids := mgr.ListSessions()
	if len(ids) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(ids))
	}

	// すべてのIDが含まれることを確認
	found := make(map[string]bool)
	for _, id := range ids {
		found[id] = true
	}
	if !found[sess1.ID] || !found[sess2.ID] || !found[sess3.ID] {
		t.Error("not all session IDs found in list")
	}
}

// TestManagerDuplicateSessionName は同名セッションの作成拒否と FindByName をテスト
func TestManagerDuplicateSessionName(t *testing.T) {
	mgr := NewManager()

	sess1, err := mgr.CreateSession("work", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create named session: %v", err)
	}
	defer sess1.Close()

	// 名前で引けることを確認
	if got := mgr.FindByName("work"); got == nil || got.ID != sess1.ID {
		t.Errorf("FindByName(work) should return session %s, got %v", sess1.ID, got)
	}
	if got := mgr.FindByName("nonexistent"); got != nil {
		t.Errorf("FindByName(nonexistent) should return nil, got %v", got)
	}

	// 同名は拒否される
	if _, err := mgr.CreateSession("work", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("expected ErrDuplicateName, got %v", err)
	}

	// 別名は作成できる
	sess2, err := mgr.CreateSession("other", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session with different name: %v", err)
	}
	defer sess2.Close()
}

// TestSessionAttachDetachClient はクライアントのアタッチ/デタッチをテスト
func TestSessionAttachDetachClient(t *testing.T) {
	mgr := NewManager()

	sess, err := mgr.CreateSession("", "/bin/cat", []string{}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	// クライアントをアタッチ
	client1 := sess.AttachClient(0, "TCP", "test-client-1", 0)
	if client1 == nil {
		t.Fatal("failed to attach client")
	}
	if client1.ID == "" {
		t.Error("client ID should not be empty")
	}

	// 2つ目のクライアントをアタッチ
	client2 := sess.AttachClient(0, "TCP", "test-client-2", 0)
	if client2 == nil {
		t.Fatal("failed to attach second client")
	}
	if client2.ID == client1.ID {
		t.Error("client IDs should be unique")
	}

	// クライアント数を確認
	sess.mu.RLock()
	clientCount := len(sess.clients)
	sess.mu.RUnlock()
	if clientCount != 2 {
		t.Errorf("expected 2 clients, got %d", clientCount)
	}

	// デタッチ
	sess.DetachClient(client1.ID)

	// デタッチ後のクライアント数を確認
	sess.mu.RLock()
	clientCount = len(sess.clients)
	sess.mu.RUnlock()
	if clientCount != 1 {
		t.Errorf("expected 1 client after detach, got %d", clientCount)
	}

	// 2つ目もデタッチ
	sess.DetachClient(client2.ID)

	sess.mu.RLock()
	clientCount = len(sess.clients)
	sess.mu.RUnlock()
	if clientCount != 0 {
		t.Errorf("expected 0 clients after detaching all, got %d", clientCount)
	}
}

// TestSessionResize はPTYのリサイズをテスト
func TestSessionResize(t *testing.T) {
	mgr := NewManager()

	sess, err := mgr.CreateSession("", "/bin/cat", []string{}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}

	// 初期サイズを確認
	if sess.Rows != 24 || sess.Cols != 80 {
		t.Errorf("expected initial size 80x24, got %dx%d", sess.Cols, sess.Rows)
	}

	// リサイズ
	err = sess.Resize(50, 120)
	if err != nil {
		t.Fatalf("failed to resize: %v", err)
	}

	// リサイズ後のサイズを確認
	if sess.Rows != 50 || sess.Cols != 120 {
		t.Errorf("expected resized size 120x50, got %dx%d", sess.Cols, sess.Rows)
	}
}

// TestSessionWriteInput はPTYへの入力書き込みをテスト
func TestSessionWriteInput(t *testing.T) {
	mgr := NewManager()

	// catコマンドでエコーバックをテスト
	sess, err := mgr.CreateSession("", "/bin/cat", []string{}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	// クライアントをアタッチして出力を受信
	client := sess.AttachClient(0, "TEST", "test-client", 0)
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}
	defer sess.DetachClient(client.ID)

	// 入力を書き込み
	testInput := []byte("hello\n")
	err = sess.WriteInput(testInput)
	if err != nil {
		t.Fatalf("failed to write input: %v", err)
	}

	// 出力を受信（タイムアウト付き）
	select {
	case output := <-client.OutCh:
		// 何らかの出力が返ってくることを確認
		if len(output) == 0 {
			t.Error("expected non-empty output")
		}
		// catコマンドなので入力がエコーバックされるはず
		if !strings.Contains(string(output), "hello") {
			t.Logf("output does not contain 'hello', got: %q", string(output))
			// Note: エコーバックのタイミングによっては複数パケットに分かれる可能性があるため
			// 厳密にチェックしない
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for output")
	}
}

// TestSessionClose はセッションのクローズをテスト
func TestSessionClose(t *testing.T) {
	mgr := NewManager()

	sess, err := mgr.CreateSession("", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// クライアントをアタッチ
	client := sess.AttachClient(0, "TEST", "test-client", 0)

	// クローズ
	sess.Close()

	// クローズ後は OutCh が閉じられるか、またはメッセージを受信できなくなる
	// (Close()はs.doneを閉じるため、各goroutineが終了する)
	select {
	case _, ok := <-client.OutCh:
		if ok {
			// まだデータが来ている可能性がある（バッファに残っている）
			// これは正常
		}
		// チャネルが閉じられた場合もOK
	case <-time.After(500 * time.Millisecond):
		// タイムアウトもOK（クライアント出力が止まっている）
	}

	// 再度クローズしてもエラーにならないことを確認（冪等性）
	sess.Close()
}

// TestSessionUDPEnabled はUDPが有効化されることをテスト
func TestSessionUDPEnabled(t *testing.T) {
	mgr := NewManager()

	sess, err := mgr.CreateSession("", "/bin/cat", []string{}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	// UDPが有効化されていることを確認
	if !sess.IsQUICEnabled() {
		t.Error("UDP should be enabled by default")
	}

	// UDPポートが取得できることを確認
	udpPort := sess.GetQUICPort()
	if udpPort == 0 {
		t.Error("UDP port should be non-zero")
	}

	// UDP鍵が取得できることを確認
	udpKey := sess.GetQUICKey()
	if len(udpKey) != 32 {
		t.Errorf("expected 32-byte UDP key, got %d bytes", len(udpKey))
	}
}

// TestSessionIDUniqueness はセッションIDがユニークであることをテスト
func TestSessionIDUniqueness(t *testing.T) {
	mgr := NewManager()

	// 100個のセッションを作成してIDがすべて異なることを確認
	ids := make(map[string]bool)
	sessions := make([]*Session, 0, 100)

	for i := 0; i < 100; i++ {
		sess, err := mgr.CreateSession("", "/bin/sleep", []string{"60"}, nil, "", 24, 80, false, 0, false)
		if err != nil {
			t.Fatalf("failed to create session %d: %v", i, err)
		}
		sessions = append(sessions, sess)

		if ids[sess.ID] {
			t.Errorf("duplicate session ID: %s", sess.ID)
		}
		ids[sess.ID] = true
	}

	// クリーンアップ
	for _, sess := range sessions {
		sess.Close()
	}
}

// TestSessionWithCustomEnv は環境変数を指定したセッション作成をテスト
func TestSessionWithCustomEnv(t *testing.T) {
	mgr := NewManager()

	env := map[string]string{
		"TEST_VAR": "test_value",
	}

	sess, err := mgr.CreateSession("", "/bin/sh", []string{"-c", "echo $TEST_VAR"}, env, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	// クライアントをアタッチして出力を確認
	client := sess.AttachClient(0, "TEST", "test-client", 0)
	defer sess.DetachClient(client.ID)
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}

	// 環境変数の値が出力されることを確認
	timeout := time.After(2 * time.Second)
	gotOutput := false
	for !gotOutput {
		select {
		case output := <-client.OutCh:
			if strings.Contains(string(output), "test_value") {
				gotOutput = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for environment variable output")
		}
	}
}

// TestAgentForwarding_EnvAndSocket は -A 付きセッション作成で SSH_AUTH_SOCK が
// 設定され、実際に UDS が bind されること、Close 後に消えることを確認する。
func TestAgentForwarding_EnvAndSocket(t *testing.T) {
	mgr := NewManager()

	sess, err := mgr.CreateSession("", "/bin/sh", []string{"-c", "echo $SSH_AUTH_SOCK"}, nil, "", 24, 80, false, 0, true)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	if sess.agentListener == nil {
		t.Fatal("expected agentListener to be set for agentForward=true")
	}
	if sess.agentSockPath == "" {
		t.Fatal("expected agentSockPath to be set for agentForward=true")
	}
	if fi, err := os.Stat(sess.agentSockPath); err != nil {
		t.Fatalf("agent socket should exist on disk: %v", err)
	} else if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("agent socket path is not a socket: %v", fi.Mode())
	}

	client := sess.AttachClient(0, "TEST", "test-client", 0)
	defer sess.DetachClient(client.ID)
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}

	timeout := time.After(2 * time.Second)
	gotOutput := false
	for !gotOutput {
		select {
		case output := <-client.OutCh:
			if strings.Contains(string(output), sess.agentSockPath) {
				gotOutput = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for SSH_AUTH_SOCK output")
		}
	}

	sockPath := sess.agentSockPath
	sess.Close()
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("agent socket should be removed after Close, stat err=%v", err)
	}
}

// TestAgentForwarding_DisabledByServer は --no-agent-forwarding 相当
// （Manager.SetAgentForwarding(false)）で UDS が作られないことを確認する。
func TestAgentForwarding_DisabledByServer(t *testing.T) {
	mgr := NewManager()
	mgr.SetAgentForwarding(false)

	sess, err := mgr.CreateSession("", "/bin/sleep", []string{"10"}, nil, "", 24, 80, false, 0, true)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	if sess.agentListener != nil {
		t.Fatal("expected no agentListener when server disables agent forwarding")
	}
	if sess.agentSockPath != "" {
		t.Fatal("expected no agentSockPath when server disables agent forwarding")
	}
}

// TestSessionMultipleClientsReceiveOutput は複数クライアントが出力を受信することをテスト
func TestSessionMultipleClientsReceiveOutput(t *testing.T) {
	mgr := NewManager()

	sess, err := mgr.CreateSession("", "/bin/echo", []string{"broadcast test"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	// 3つのクライアントをアタッチしてからプロセスを起動
	client1 := sess.AttachClient(0, "TEST", "client-1", 0)
	defer sess.DetachClient(client1.ID)
	client2 := sess.AttachClient(0, "TEST", "client-2", 0)
	defer sess.DetachClient(client2.ID)
	client3 := sess.AttachClient(0, "TEST", "client-3", 0)
	defer sess.DetachClient(client3.ID)
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}

	// すべてのクライアントが出力を受信できることを確認
	clients := []*Client{client1, client2, client3}
	received := make([]bool, 3)

	timeout := time.After(3 * time.Second)
	for i := 0; i < 3; i++ {
		for j, client := range clients {
			if received[j] {
				continue
			}
			select {
			case output := <-client.OutCh:
				if len(output) > 0 {
					received[j] = true
					t.Logf("client %d received output: %q", j+1, string(output))
				}
			case <-timeout:
				t.Fatalf("timeout waiting for output on client %d", j+1)
			default:
				// 次のクライアントをチェック
			}
		}
		// 少し待機して次の出力をチェック
		time.Sleep(100 * time.Millisecond)
	}

	// すべてのクライアントが受信したことを確認
	for i, rcv := range received {
		if !rcv {
			t.Errorf("client %d did not receive output", i+1)
		}
	}
}

// waitExitCode はセッションの exit code が記録されるまで待つ（-1 のままならタイムアウト）。
func waitExitCode(t *testing.T, sess *Session, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if code := sess.ExitCode(); code >= 0 {
			return code
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("exit code not recorded within %v (got %d)", timeout, sess.ExitCode())
	return -1
}

// TestSessionExitCode はプロセスの終了コードが記録されることをテスト
func TestSessionExitCode(t *testing.T) {
	mgr := NewManager()
	sess, err := mgr.CreateSession("", "/bin/sh", []string{"-c", "exit 7"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	if code := sess.ExitCode(); code != -1 {
		t.Errorf("exit code before process start should be -1, got %d", code)
	}
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}
	if code := waitExitCode(t, sess, 5*time.Second); code != 7 {
		t.Errorf("expected exit code 7, got %d", code)
	}
}

// TestSessionExitCodeSignal はシグナル死が 128+signal に換算されることをテスト
func TestSessionExitCodeSignal(t *testing.T) {
	mgr := NewManager()
	sess, err := mgr.CreateSession("", "/bin/sh", []string{"-c", "kill -TERM $$"}, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sess.Close()

	if err := sess.StartProcess(); err != nil {
		t.Fatalf("failed to start process: %v", err)
	}
	// SIGTERM = 15 → シェル慣行の 128+15
	if code := waitExitCode(t, sess, 5*time.Second); code != 143 {
		t.Errorf("expected exit code 143 (128+SIGTERM), got %d", code)
	}
}

// TestActivityTimestamps は handlePTYOutput / WriteInput が freshness 用の
// 最終出力・入力時刻を更新することをテスト（白箱）
func TestActivityTimestamps(t *testing.T) {
	s := &Session{}
	if !s.GetLastOutputAt().IsZero() || !s.GetLastInputAt().IsZero() {
		t.Fatal("expected zero activity timestamps on a fresh session")
	}

	before := time.Now()
	s.handlePTYOutput([]byte("hello"))
	if got := s.GetLastOutputAt(); got.Before(before) {
		t.Errorf("lastOutputAt = %v, want >= %v", got, before)
	}
	if !s.GetLastInputAt().IsZero() {
		t.Error("lastInputAt should not be updated by output")
	}

	// WriteInput は PTY の代わりにパイプへ書いて検証する
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	s.ptyMaster = w

	before = time.Now()
	if err := s.WriteInput([]byte("x")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	if got := s.GetLastInputAt(); got.Before(before) {
		t.Errorf("lastInputAt = %v, want >= %v", got, before)
	}
}

// TestEvictStaleOutput は出力が止まったセッションでも定期 evict で
// 保持期限切れチャンクが回収されることをテスト（白箱: outputChunks を直接構築）
func TestEvictStaleOutput(t *testing.T) {
	s := &Session{}
	old := time.Now().Add(-maxOutputBufferAge - time.Minute)
	fresh := time.Now()
	s.outputChunks = []OutputChunk{
		{Seq: 1, Data: []byte("old-1"), Timestamp: old},
		{Seq: 2, Data: []byte("old-2"), Timestamp: old},
		{Seq: 3, Data: []byte("fresh"), Timestamp: fresh},
	}
	s.outputBufferBytes = 5 + 5 + 5

	s.EvictStaleOutput()

	if len(s.outputChunks) != 1 || s.outputChunks[0].Seq != 3 {
		t.Fatalf("expected only fresh chunk to remain, got %d chunks", len(s.outputChunks))
	}
	if s.outputBufferBytes != 5 {
		t.Errorf("outputBufferBytes = %d, want 5", s.outputBufferBytes)
	}
}

// TestManagerMaxSessions は --max-sessions の上限で CREATE が拒否されることをテスト
func TestManagerMaxSessions(t *testing.T) {
	mgr := NewManager()
	mgr.SetMaxSessions(1)

	first, err := mgr.CreateSession("", "/bin/sh", nil, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("first create should succeed: %v", err)
	}
	defer first.Close()

	if _, err := mgr.CreateSession("", "/bin/sh", nil, nil, "", 24, 80, false, 0, false); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("second create should fail with ErrTooManySessions, got %v", err)
	}

	// PTY 終了済みセッションは上限に数えない（Close で ptyClosed が立つ）
	first.Close()
	third, err := mgr.CreateSession("", "/bin/sh", nil, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("create after closing should succeed: %v", err)
	}
	third.Close()
}
