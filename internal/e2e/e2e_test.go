//go:build e2e

// Package e2e は実バイナリ（tezzerd / tezzer）を起動して pty 経由で動作確認する
// 最小の E2E スモークテスト。
//
// プロトコルの正しさは L2/L3 のシミュレーションが担保するので、ここでは
// 「起動 → セッション作成 → 出力が end-to-end で流れる → 入力エコー → -list → 終了」
// という配線だけを確認する。実時間・実 PTY に依存しフレークしうるため build tag を付け、
// `make e2e` でのみ実行する（make test / make ci には含めない）。
package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestE2ESmoke(t *testing.T) {
	tezzerd, tezzer := tezzerBinaries(t)

	sock := tempSocket(t)

	// STUN は隔離環境で google に到達できないため、即座に失敗する不達アドレスを指定
	// （NAT traversal は使わず UDS 経由で動く。STUN 失敗はセッション動作に影響しない）。
	srv := exec.Command(tezzerd, "-listen-unix", sock, "-stun-server", "127.0.0.1:1")
	srv.Env = append(os.Environ(), "TERM=xterm")
	if err := srv.Start(); err != nil {
		t.Fatalf("start tezzerd: %v", err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	}()

	if !waitForFile(sock, 5*time.Second) {
		t.Fatal("tezzerd did not create socket in time")
	}

	// クライアントを pty 配下で起動。セッションは sh -c 'echo MARKER; cat'。
	// MARKER の出力で「セッション出力が end-to-end で流れる」ことを確認し、
	// その後 cat で入力エコー往復を確認する。
	const marker = "E2E_READY_MARKER"
	cli := exec.Command(tezzer, "-addr-unix", sock, "-cmd", "/bin/sh", "--", "-c", "echo "+marker+"; cat")
	cli.Env = append(os.Environ(), "TERM=xterm")
	ptmx, err := pty.Start(cli)
	if err != nil {
		t.Fatalf("pty.Start(tezzer): %v", err)
	}
	defer func() {
		_ = ptmx.Close()
		_ = cli.Process.Kill()
		_, _ = cli.Process.Wait()
	}()
	ptySize(ptmx)

	reader := newPtyReader(ptmx)

	// (1) セッション出力が届く
	reader.waitFor(t, marker, 15*time.Second)

	// (2) 入力エコー往復（cat が stdin を stdout に返す）
	const ping = "PINGPONG12345"
	if _, err := ptmx.Write([]byte(ping + "\n")); err != nil {
		t.Fatalf("write to pty: %v", err)
	}
	reader.waitFor(t, ping, 15*time.Second)

	// (3) -list に作成したセッションが現れる
	listOut := runList(t, tezzer, sock)
	if !strings.Contains(listOut, "SESSION ID") {
		t.Fatalf("-list output missing header:\n%s", listOut)
	}
	if !strings.Contains(listOut, "sh") {
		t.Fatalf("-list output missing the sh session:\n%s", listOut)
	}
}

// TestE2EExitCode はセッションプロセスの終了コードがクライアントの終了コードへ
// 伝搬されること（ssh と同じ挙動）を実バイナリで確認する。
func TestE2EExitCode(t *testing.T) {
	tezzerd, tezzer := tezzerBinaries(t)

	sock := tempSocket(t)

	srv := exec.Command(tezzerd, "-listen-unix", sock, "-stun-server", "127.0.0.1:1")
	srv.Env = append(os.Environ(), "TERM=xterm")
	if err := srv.Start(); err != nil {
		t.Fatalf("start tezzerd: %v", err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	}()

	if !waitForFile(sock, 5*time.Second) {
		t.Fatal("tezzerd did not create socket in time")
	}

	const marker = "E2E_EXIT_MARKER"
	cli := exec.Command(tezzer, "-addr-unix", sock, "-cmd", "/bin/sh", "--", "-c", "echo "+marker+"; exit 7")
	cli.Env = append(os.Environ(), "TERM=xterm")
	ptmx, err := pty.Start(cli)
	if err != nil {
		t.Fatalf("pty.Start(tezzer): %v", err)
	}
	defer func() { _ = ptmx.Close() }()
	ptySize(ptmx)

	reader := newPtyReader(ptmx)
	reader.waitFor(t, marker, 15*time.Second)

	// セッションプロセスの exit 7 がクライアントの終了コードになる
	waitCh := make(chan error, 1)
	go func() { waitCh <- cli.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(20 * time.Second):
		_ = cli.Process.Kill()
		t.Fatalf("client did not exit in time; pty output:\n%s", reader.snapshot())
	}
	if code := cli.ProcessState.ExitCode(); code != 7 {
		t.Fatalf("client exit code = %d, want 7; pty output:\n%s", code, reader.snapshot())
	}
}

// TestE2EWait は -wait がセッションのコマンド終了を待ち、exit code を伝搬する
// ことを実バイナリで確認する。attach せずに（detach 中のセッションを）待てるのが
// 主目的なので、セッション作成はバックグラウンドの attach クライアントで行い、
// -wait は別プロセスとして起動する。
func TestE2EWait(t *testing.T) {
	tezzerd, tezzer := tezzerBinaries(t)

	sock := tempSocket(t)

	srv := exec.Command(tezzerd, "-listen-unix", sock, "-stun-server", "127.0.0.1:1")
	srv.Env = append(os.Environ(), "TERM=xterm")
	if err := srv.Start(); err != nil {
		t.Fatalf("start tezzerd: %v", err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	}()

	if !waitForFile(sock, 5*time.Second) {
		t.Fatal("tezzerd did not create socket in time")
	}

	// セッション作成: 2 秒後に exit 7 する名前付きセッション
	const marker = "E2E_WAIT_MARKER"
	cli := exec.Command(tezzer, "-addr-unix", sock, "-name", "waittest",
		"-cmd", "/bin/sh", "--", "-c", "echo "+marker+"; sleep 2; exit 7")
	cli.Env = append(os.Environ(), "TERM=xterm")
	ptmx, err := pty.Start(cli)
	if err != nil {
		t.Fatalf("pty.Start(tezzer): %v", err)
	}
	defer func() {
		_ = ptmx.Close()
		_ = cli.Process.Kill()
		_, _ = cli.Process.Wait()
	}()
	ptySize(ptmx)

	reader := newPtyReader(ptmx)
	reader.waitFor(t, marker, 15*time.Second)

	// セッションが生きているうちに -wait -name で終了を待つ
	waiter := exec.Command(tezzer, "-addr-unix", sock, "-wait", "-name", "waittest")
	waiter.Env = append(os.Environ(), "TERM=xterm")
	waitCh := make(chan error, 1)
	if err := waiter.Start(); err != nil {
		t.Fatalf("start tezzer -wait: %v", err)
	}
	go func() { waitCh <- waiter.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(20 * time.Second):
		_ = waiter.Process.Kill()
		t.Fatalf("-wait did not return in time; attach pty output:\n%s", reader.snapshot())
	}
	if code := waiter.ProcessState.ExitCode(); code != 7 {
		t.Fatalf("-wait exit code = %d, want 7", code)
	}

	// 存在しないセッション名は即エラー（exit 1）
	waiterNG := exec.Command(tezzer, "-addr-unix", sock, "-wait", "-name", "no-such-name")
	waiterNG.Env = append(os.Environ(), "TERM=xterm")
	out, err := waiterNG.CombinedOutput()
	if err == nil {
		t.Fatalf("-wait on missing session should fail, got success:\n%s", out)
	}
	if code := waiterNG.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("-wait on missing session: exit code = %d, want 1\n%s", code, out)
	}
}

// TestE2EPeek は -peek（読み取り専用 attach）が「出力は見えるが入力は届かない」
// ことを実バイナリで確認する。cat セッションに通常 attach と -peek attach を並走させ、
// peek 側の打鍵がエコーされない（= PTY に届いていない）ことと、通常側の入力による
// 出力が peek 側にも届くことを見る。
func TestE2EPeek(t *testing.T) {
	tezzerd, tezzer := tezzerBinaries(t)

	sock := tempSocket(t)

	srv := exec.Command(tezzerd, "-listen-unix", sock, "-stun-server", "127.0.0.1:1")
	srv.Env = append(os.Environ(), "TERM=xterm")
	if err := srv.Start(); err != nil {
		t.Fatalf("start tezzerd: %v", err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	}()

	if !waitForFile(sock, 5*time.Second) {
		t.Fatal("tezzerd did not create socket in time")
	}

	// 通常 attach: cat セッション（入力をエコーする）
	const marker = "E2E_PEEK_MARKER"
	cli := exec.Command(tezzer, "-addr-unix", sock, "-name", "peektest",
		"-cmd", "/bin/sh", "--", "-c", "echo "+marker+"; cat")
	cli.Env = append(os.Environ(), "TERM=xterm")
	ptmx, err := pty.Start(cli)
	if err != nil {
		t.Fatalf("pty.Start(tezzer): %v", err)
	}
	defer func() {
		_ = ptmx.Close()
		_ = cli.Process.Kill()
		_, _ = cli.Process.Wait()
	}()
	ptySize(ptmx)
	mainReader := newPtyReader(ptmx)
	mainReader.waitFor(t, marker, 15*time.Second)

	// -peek attach
	peeker := exec.Command(tezzer, "-addr-unix", sock, "-peek", "-name", "peektest")
	peeker.Env = append(os.Environ(), "TERM=xterm")
	peekPtmx, err := pty.Start(peeker)
	if err != nil {
		t.Fatalf("pty.Start(tezzer -peek): %v", err)
	}
	defer func() {
		_ = peekPtmx.Close()
		_ = peeker.Process.Kill()
		_, _ = peeker.Process.Wait()
	}()
	ptySize(peekPtmx)
	peekReader := newPtyReader(peekPtmx)

	// (1) peek 側にもバックログ（MARKER）が届く
	peekReader.waitFor(t, marker, 15*time.Second)

	// (2) peek 側の打鍵は PTY に届かない（cat がエコーしない）
	const stray = "STRAYKEYS999"
	if _, err := peekPtmx.Write([]byte(stray + "\n")); err != nil {
		t.Fatalf("write to peek pty: %v", err)
	}
	// 対照実験を先に流す: 通常側の打鍵はエコーされ、両クライアントに届く
	const visible = "VISIBLE12345"
	if _, err := ptmx.Write([]byte(visible + "\n")); err != nil {
		t.Fatalf("write to main pty: %v", err)
	}
	mainReader.waitFor(t, visible, 15*time.Second)
	peekReader.waitFor(t, visible, 15*time.Second)
	// visible が往復した時点で、それより先に送った stray が PTY に届いていれば
	// エコーが観測されているはず。どちらの画面にも現れないことを確認
	if strings.Contains(mainReader.snapshot(), stray) || strings.Contains(peekReader.snapshot(), stray) {
		t.Fatalf("peek input leaked into the session:\nmain:\n%s\npeek:\n%s",
			mainReader.snapshot(), peekReader.snapshot())
	}

	// (3) peek 側はエスケープキー（Ctrl-^ .）で detach でき、exit 0
	if _, err := peekPtmx.Write([]byte{0x1e, '.'}); err != nil {
		t.Fatalf("write detach sequence: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- peeker.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(15 * time.Second):
		_ = peeker.Process.Kill()
		t.Fatalf("peek client did not detach in time; output:\n%s", peekReader.snapshot())
	}
	if code := peeker.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("peek detach exit code = %d, want 0", code)
	}
}

// runList は tezzer -list を実行して出力を返す。
func runList(t *testing.T, tezzer, sock string) string {
	t.Helper()
	cmd := exec.Command(tezzer, "-addr-unix", sock, "-list")
	cmd.Env = append(os.Environ(), "TERM=xterm")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tezzer -list failed: %v\n%s", err, out)
	}
	return string(out)
}
