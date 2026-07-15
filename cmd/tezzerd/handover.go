package main

// handover.go: 無停止再起動（self re-exec）の tezzerd 側。
// 状態のシリアライズ・復元は internal/session/handover.go、ここは
// 「一時ファイルへの書き出し → fd 継承の準備 → execve」のオーケストレーション。
//
// トリガーは SIGUSR2。exec はプロセス置き換えなので PID・子プロセスの親子関係が
// 維持される（exit code 回収が壊れない）。UDS リスナーは CLOEXEC で exec 時に閉じ、
// 新プロセスの通常起動パス（stale socket 検出 → 再 bind）がそのまま面倒を見る。
// 接続中クライアントの QUIC 接続は一度切れるが、ポートと共有鍵は継承されるため
// クライアントの既存 reconnect 機構で自動復旧する。

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kuriyama/tezzer/internal/session"
)

// performHandover は状態を書き出して自分自身を exec で置き換える。
// 成功時はこの関数から戻らない（プロセスが置き換わる）。失敗時はロック・fd を
// 解放して err を返し、呼び出し元は通常運転を継続できる。
func performHandover(mgr *session.Manager) error {
	exe := resolveSelfExecutable()

	// 状態ファイルは tmpfs（XDG_RUNTIME_DIR）優先。作成後すぐ unlink して fd だけで持つ。
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "tezzerd-handover-*")
	if err != nil {
		return fmt.Errorf("create state file: %w", err)
	}
	_ = os.Remove(f.Name())

	// ここから先、成功パスでは全セッションのロックを保持したまま exec する
	// （シリアライズ後に PTY 出力が進んで seq がずれるのを防ぐ world stop）。
	abort, err := mgr.WriteHandover(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("serialize state: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		abort()
		f.Close()
		return fmt.Errorf("seek state file: %w", err)
	}
	// dup で CLOEXEC を外した継承用 fd を作る（f 自体は CLOEXEC 付きなので exec で閉じる）。
	stateFd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		abort()
		f.Close()
		return fmt.Errorf("dup state fd: %w", err)
	}

	env := append(os.Environ(), fmt.Sprintf("%s=%d", session.HandoverEnvVar, stateFd))
	log.Printf("handover: exec %s (state fd=%d)", exe, stateFd)
	err = syscall.Exec(exe, os.Args, env)
	// ここに到達するのは exec 失敗時のみ。
	_ = syscall.Close(stateFd)
	f.Close()
	abort()
	return fmt.Errorf("exec %s: %w", exe, err)
}

// resolveSelfExecutable は re-exec すべきバイナリのパスを返す。
// /proc/self/exe（os.Executable）は「起動時の inode」を指すため、ディスク上の
// バイナリが更新されている場合に旧バイナリ（deleted）へ解決されてしまう。
// アップグレード目的の再起動では新バイナリを実行したいので、argv[0] を
// パス解決した結果を優先する。
func resolveSelfExecutable() string {
	arg0 := os.Args[0]
	if strings.Contains(arg0, "/") {
		if abs, err := filepath.Abs(arg0); err == nil {
			return abs
		}
		return arg0
	}
	if p, err := exec.LookPath(arg0); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	// 最終フォールバックはリテラルの /proc/self/exe。os.Executable() の返す
	// パス文字列だと、バイナリが rename で置き換え済みの場合に「... (deleted)」と
	// なって exec に失敗するが、/proc/self/exe そのものはカーネルが旧 inode を
	// 解決するため exec できる（アップグレードにはならないが、handover 自体は
	// 旧バイナリの再実行として完遂できる）。
	return "/proc/self/exe"
}
