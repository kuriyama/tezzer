//go:build e2e_docker

// スリープ復帰の Docker シナリオ E2E（手動テスト）。
//
// クライアントプロセスを SIGSTOP で実時間 30 秒超フリーズし、SIGCONT で復帰させる。
// これは synctest（仮想時間）では原理的に踏めない経路 ——
//
//	(a) keepAliveMonitor.watchLoop のスリープ検出（elapsed > checkInterval*5 = 30s）
//	(b) recoverWithSocketRebind による実 UDP ソケットの再作成
//
// —— を実カーネル・実時間で通す。復帰後に出力ストリームが再開することを確認する。
//
// 手動専用（`make e2e-docker`）。KA 間隔は既定 3 秒固定（バイナリに調整フラグなし）なので
// スリープ検出閾値 30 秒を超えるフリーズが必要で、1 本で 40〜50 秒かかる。
package e2e

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

var tickRe = regexp.MustCompile(`TICK-(\d+)`)

// maxTick は出力中の "TICK-<n>" の最大 n を返す（無ければ -1）。
func maxTick(s string) int {
	max := -1
	for _, m := range tickRe.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

func TestE2EDockerSleepRecovery(t *testing.T) {
	tezzerd, tezzer := tezzerBinaries(t)
	sock := tempSocket(t)

	// サーバーは固定 UDP ポートで起動（loopback で確実に到達できるように）
	srv := exec.Command(tezzerd, "-listen-unix", sock, "-stun-server", "127.0.0.1:1", "-udp-port", "7777")
	srv.Env = append(os.Environ(), "TERM=xterm")
	if err := srv.Start(); err != nil {
		t.Fatalf("start tezzerd: %v", err)
	}
	defer func() { _ = srv.Process.Kill(); _, _ = srv.Process.Wait() }()

	if !waitForFile(sock, 5*time.Second) {
		t.Fatal("tezzerd did not create socket in time")
	}

	// セッションは 1 秒ごとに連番 TICK を出し続ける。これでフリーズ前後の出力進行を観測できる。
	const sessionCmd = "i=0; while true; do echo TICK-$i; i=$((i+1)); sleep 1; done"
	cli := exec.Command(tezzer, "-addr-unix", sock, "-cmd", "/bin/sh", "--", "-c", sessionCmd)
	cli.Env = append(os.Environ(), "TERM=xterm")
	ptmx, err := pty.Start(cli)
	if err != nil {
		t.Fatalf("pty.Start(tezzer): %v", err)
	}
	defer func() { _ = ptmx.Close(); _ = cli.Process.Kill(); _, _ = cli.Process.Wait() }()
	ptySize(ptmx)

	reader := newPtyReader(ptmx)

	// フリーズ前に出力が流れていることを確認
	reader.waitFor(t, "TICK-3", 20*time.Second)
	beforeMax := maxTick(reader.snapshot())
	t.Logf("before freeze: max tick = %d", beforeMax)

	// クライアントプロセスをフリーズ（= ラップトップのスリープ相当）。
	// スリープ検出閾値 30 秒を超えるよう 35 秒フリーズする。サーバーは動き続ける。
	const freeze = 35 * time.Second
	if err := syscall.Kill(cli.Process.Pid, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	t.Logf("client frozen for %v (server keeps running)...", freeze)
	time.Sleep(freeze)
	if err := syscall.Kill(cli.Process.Pid, syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT: %v", err)
	}
	t.Log("client resumed; expecting recovery + output resume")

	// 復帰後、出力ストリームが再開すること（フリーズ前より新しい TICK が届く）を確認
	deadline := time.Now().Add(45 * time.Second)
	var resumedMax int = -1
	for time.Now().Before(deadline) {
		if m := maxTick(reader.snapshot()); m > beforeMax {
			resumedMax = m
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if resumedMax < 0 {
		t.Fatalf("output did not resume after wake (still max=%d, beforeMax=%d):\n%s",
			maxTick(reader.snapshot()), beforeMax, tailLog(reader.snapshot()))
	}
	t.Logf("output resumed: max tick = %d", resumedMax)

	// さらにストリームが生きている（継続して新しい TICK が届く）ことを確認
	time.Sleep(4 * time.Second)
	if liveMax := maxTick(reader.snapshot()); liveMax <= resumedMax {
		t.Fatalf("stream not live after recovery: resumedMax=%d liveMax=%d", resumedMax, liveMax)
	}
}

// tailLog は失敗時の診断用に末尾 1KB を返す。
func tailLog(s string) string {
	if len(s) > 1024 {
		return s[len(s)-1024:]
	}
	return s
}
