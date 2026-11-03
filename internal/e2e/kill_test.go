//go:build e2e

package e2e

// kill による終了経路（Ctrl-^ q / 別クライアントからの -kill）で、attach 中の
// クライアントが固まらず終了し、終了コードが期待どおりであることを確認する。

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
)

// startServerAndClient は tezzerd と、名前付きセッション（sh -c 'echo MARKER; cat'）に
// attach した tezzer を pty 配下で起動する。
func startServerAndClient(t *testing.T, name, marker string) (tezzer, sock string, cli *exec.Cmd, ptmx *os.File, reader *ptyReader) {
	t.Helper()
	tezzerd, tezzer := tezzerBinaries(t)
	sock = tempSocket(t)

	srv := exec.Command(tezzerd, "-listen-unix", sock, "-stun-server", "127.0.0.1:1")
	srv.Env = append(os.Environ(), "TERM=xterm")
	if err := srv.Start(); err != nil {
		t.Fatalf("start tezzerd: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	})
	if !waitForFile(sock, 5*time.Second) {
		t.Fatal("tezzerd did not create socket in time")
	}

	cli = exec.Command(tezzer, "-addr-unix", sock, "-name", name, "-cmd", "/bin/sh", "--", "-c", "echo "+marker+"; cat")
	cli.Env = append(os.Environ(), "TERM=xterm")
	var err error
	ptmx, err = pty.Start(cli)
	if err != nil {
		t.Fatalf("pty.Start(tezzer): %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	ptySize(ptmx)
	reader = newPtyReader(ptmx)
	reader.waitFor(t, marker, 15*time.Second)
	return tezzer, sock, cli, ptmx, reader
}

// waitExit はクライアントの終了を待って終了コードを返す（固まったら失敗）。
func waitExit(t *testing.T, cli *exec.Cmd, reader *ptyReader) int {
	t.Helper()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cli.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(20 * time.Second):
		_ = cli.Process.Kill()
		t.Fatalf("client did not exit in time; pty output:\n%s", reader.snapshot())
	}
	return cli.ProcessState.ExitCode()
}

// TestE2EKillByEscape は Ctrl-^ q でセッションを kill したクライアントが exit 0 で
// 終わることを確認する（kill の結果届く sessionGone はエラー扱いしない）。
func TestE2EKillByEscape(t *testing.T) {
	_, _, cli, ptmx, reader := startServerAndClient(t, "e2e-kill-esc", "E2E_KILL_ESC_MARKER")

	if _, err := ptmx.Write([]byte{0x1e, 'q'}); err != nil {
		t.Fatalf("write escape: %v", err)
	}
	reader.waitFor(t, "killed", 10*time.Second)
	if code := waitExit(t, cli, reader); code != 0 {
		t.Fatalf("exit code = %d, want 0; pty output:\n%s", code, reader.snapshot())
	}
}

// TestE2EKillFromOtherClient は別クライアントの -kill で、attach 中のクライアントが
// "session killed" を表示して exit 0 で終わることを確認する。kill は明示的な終了指示なので、
// QUIC の sessionGone と UDS の SESSION_CLOSED のどちらが先着しても同じ結果になる。
func TestE2EKillFromOtherClient(t *testing.T) {
	tezzer, sock, cli, _, reader := startServerAndClient(t, "e2e-kill-other", "E2E_KILL_OTHER_MARKER")

	out, err := exec.Command(tezzer, "-addr-unix", sock, "-list", "-json").Output()
	if err != nil {
		t.Fatalf("-list -json: %v", err)
	}
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &list); err != nil || len(list.Sessions) != 1 {
		t.Fatalf("-list -json: want 1 session (err=%v): %s", err, out)
	}
	if out, err := exec.Command(tezzer, "-addr-unix", sock, "-kill", list.Sessions[0].SessionID).CombinedOutput(); err != nil {
		t.Fatalf("-kill: %v\n%s", err, out)
	}
	reader.waitFor(t, "session killed", 10*time.Second)
	if code := waitExit(t, cli, reader); code != 0 {
		t.Fatalf("exit code = %d, want 0; pty output:\n%s", code, reader.snapshot())
	}
}
