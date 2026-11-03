package termui

import "testing"

// Close 後の Set は描かない（終了時に端末を戻した後の画面へステータス行を残さない）。
func TestStatusManagerIgnoresSetAfterClose(t *testing.T) {
	var shown []string
	sm := NewStatusManager(func(msg string) { shown = append(shown, msg) }, func() {})
	sm.Set("before")
	sm.Close()
	sm.Set("after")
	if len(shown) != 1 || shown[0] != "before" {
		t.Errorf("shown = %q, want [before]", shown)
	}
}
