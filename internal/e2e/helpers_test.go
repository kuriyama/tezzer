//go:build e2e || e2e_docker

// e2e / e2e_docker 共通のヘルパー。
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// tezzerBinaries は tezzerd / tezzer の実行パスを返す。
// 環境変数 TEZZER_E2E_BINDIR が指定されていればそこの事前ビルド済みバイナリを使い
// （Docker コンテナ内で Go ツールチェーンなしに動かすため）、なければその場でビルドする。
func tezzerBinaries(t *testing.T) (tezzerd, tezzer string) {
	t.Helper()
	if dir := os.Getenv("TEZZER_E2E_BINDIR"); dir != "" {
		return filepath.Join(dir, "tezzerd"), filepath.Join(dir, "tezzer")
	}
	out := t.TempDir()
	return buildBinary(t, "tezzerd", out), buildBinary(t, "tezzer", out)
}

// buildBinary は cmd/<name> をテンポラリにビルドしてパスを返す。
func buildBinary(t *testing.T, name, outDir string) string {
	t.Helper()
	out := filepath.Join(outDir, name)
	cmd := exec.Command("go", "build", "-o", out, "./cmd/"+name)
	cmd.Dir = repoRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s failed: %v\n%s", name, err, b)
	}
	return out
}

// repoRoot はこのテストパッケージ（internal/e2e）から見たリポジトリルートを返す。
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// ptyReader は pty マスターを非同期に読み続けて蓄積し、部分文字列の出現を待てるようにする。
type ptyReader struct {
	mu  sync.Mutex
	buf []byte
}

func newPtyReader(f *os.File) *ptyReader {
	r := &ptyReader{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := f.Read(b)
			if n > 0 {
				r.mu.Lock()
				r.buf = append(r.buf, b[:n]...)
				r.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

func (r *ptyReader) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// waitFor は sub が蓄積出力に現れるまで待つ。現れなければ Fatal。
func (r *ptyReader) waitFor(t *testing.T, sub string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(r.snapshot(), sub) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %q in pty output; got:\n%s", sub, r.snapshot())
}

// tempSocket は一時ディレクトリ内のソケットパスを返す。
// UDS のパス長制限（約 100 文字）に収まるよう短いパスにする。
func tempSocket(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "t.sock")
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// ptySize は端末サイズを設定する（best-effort）。
func ptySize(f *os.File) {
	_ = pty.Setsize(f, &pty.Winsize{Rows: 24, Cols: 80})
}
