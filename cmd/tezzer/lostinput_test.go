package main

import (
	"strings"
	"testing"

	"github.com/kuriyama/tezzer/internal/termui"
)

func TestLostInputPreview(t *testing.T) {
	tests := []struct {
		name string
		head string
		more bool
		want string
	}{
		{"enter and tab", "ls\tx\r", false, "ls⇥x⏎"},
		{"control chars", "a\x03b\x1b[Ac\x7f", false, "a^Cb^[[Ac^?"},
		{"mouse and focus reports are dropped", "\x1b[<35;10;5Mvi\x1b[M !!\x1b[I\x1b[O\n", false, "vi⏎"},
		{"truncated", "make deploy-production\n", false, "make deploy-prod…"},
		{"more than kept", "ls", true, "ls…"},
		{"utf-8", "日本語\n", false, "日本語⏎"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lostInputPreview([]byte(tt.head), tt.more); got != tt.want {
				t.Errorf("lostInputPreview(%q, %v) = %q, want %q", tt.head, tt.more, got, tt.want)
			}
		})
	}
}

// 自動通知はバイト数だけ（パスワードが混ざりうるため）。冒頭は Ctrl-^ i でだけ出す。
func TestOnInputLost_AutoNoticeHasCountOnly(t *testing.T) {
	var shown []string
	c := &Client{done: make(chan struct{}), escapeByte: 0x1e, sessionID: "avHB52L2d4MmTJa"}
	c.statusMgr = termui.NewStatusManager(func(msg string) { shown = append(shown, msg) }, func() {})
	t.Cleanup(c.statusMgr.Close)

	c.onInputLost(23, []byte("secret-pass\nls\n"))
	if len(shown) != 1 || strings.Contains(shown[0], "secret") || !strings.Contains(shown[0], "23 bytes") {
		t.Fatalf("auto notice = %q, want the byte count only", shown)
	}
	c.showStatus()
	if len(shown) != 2 || !strings.Contains(shown[1], `lost:"secret-pass⏎ls⏎…"(23B)`) {
		t.Errorf("Ctrl-^ i = %q, want it to include the beginning of the lost input", shown[len(shown)-1])
	}
	for _, e := range c.getLogMessages() {
		if strings.Contains(e.message, "secret") {
			t.Errorf("log has the lost input: %q", e.message)
		}
	}
}
