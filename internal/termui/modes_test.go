package termui

import "testing"

func TestModeTracker(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"initial", nil, ""},
		{"plain text", []string{"hello\r\nworld"}, ""},
		{
			// zellij の起動時相当: alt screen・マウス（複数パラメータ）・bracketed paste・カーソル非表示
			"zellij like",
			[]string{"\x1b[?1049h\x1b[?1000;1002;1006;1015h\x1b[?2004h\x1b[?25l"},
			"\x1b[?1049h\x1b[?1000h\x1b[?1002h\x1b[?1006h\x1b[?1015h\x1b[?2004h\x1b[?25l",
		},
		{"set then reset", []string{"\x1b[?1000h", "text", "\x1b[?1000l"}, ""},
		{
			"split across chunks",
			[]string{"\x1b", "[?10", "06", ";2004", "h"},
			"\x1b[?1006h\x1b[?2004h",
		},
		{"keypad", []string{"\x1b="}, "\x1b="},
		{"keypad off", []string{"\x1b=", "\x1b>"}, ""},
		{"RIS resets all", []string{"\x1b[?1049h\x1b[?1000h\x1b=\x1b[?25l", "\x1bc"}, ""},
		{"cursor shown is default", []string{"\x1b[?25l\x1b[?25h"}, ""},
		{"untracked mode ignored", []string{"\x1b[?7l\x1b[?2026h"}, ""},
		{"non-private CSI ignored", []string{"\x1b[1000h\x1b[2004h"}, ""},
		{"intermediate byte is not DECSET", []string{"\x1b[?2004$p"}, ""},
		{"kitty query ignored", []string{"\x1b[?u\x1b[>1u"}, ""},
		{"CAN aborts sequence", []string{"\x1b[?1000\x18h"}, ""},
		{"ESC restarts sequence", []string{"\x1b[?10\x1b[?2004h"}, "\x1b[?2004h"},
		{"alt screen first in restore", []string{"\x1b[?2004h\x1b[?1049h"}, "\x1b[?1049h\x1b[?2004h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr ModeTracker
			for _, c := range tt.chunks {
				tr.Scan([]byte(c))
			}
			if got := string(tr.Restore()); got != tt.want {
				t.Errorf("Restore() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Restore の出力を新しいトラッカーに食わせると同じ状態になる（handover での引き継ぎに使う）。
func TestModeTrackerRestoreRoundTrip(t *testing.T) {
	var a ModeTracker
	a.Scan([]byte("\x1b[?1049h\x1b[?1002;1006h\x1b[?1h\x1b=\x1b[?25l"))
	var b ModeTracker
	b.Scan(a.Restore())
	if got, want := string(b.Restore()), string(a.Restore()); got != want {
		t.Errorf("round trip = %q, want %q", got, want)
	}
}

// ベンチマーク用の出力（4KB 前後 = PTY 読み取り 1 回分の典型サイズ）。
func benchOutputs() map[string][]byte {
	var plain, tui, mixed []byte
	for len(plain) < 4096 {
		plain = append(plain, "drwxr-xr-x  5 user group  4096 Oct  9 16:00 some-directory-name\r\n"...)
	}
	// zellij/htop の再描画相当: カーソル移動 + SGR が数文字ごとに入る
	for len(tui) < 4096 {
		tui = append(tui, "\x1b[12;40H\x1b[38;5;245m\x1b[48;5;236m CPU \x1b[1;32m42%\x1b[0m\x1b[K"...)
	}
	// ログ出力に色付けがときどき入る程度
	for len(mixed) < 4096 {
		mixed = append(mixed, "2026-10-09 16:00:00 \x1b[32mINFO\x1b[0m request handled in 12ms path=/api/v1/items\r\n"...)
	}
	return map[string][]byte{"plain": plain, "tui": tui, "mixed": mixed}
}

func BenchmarkModeTrackerScan(b *testing.B) {
	for name, data := range benchOutputs() {
		b.Run(name, func(b *testing.B) {
			var tr ModeTracker
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				tr.Scan(data)
			}
		})
	}
}

// 比較用: handlePTYOutput が毎チャンク行っているコピー（make + copy）のコスト。
func BenchmarkCopyBaseline(b *testing.B) {
	data := benchOutputs()["tui"]
	b.SetBytes(int64(len(data)))
	var sink []byte
	for b.Loop() {
		sink = make([]byte, len(data))
		copy(sink, data)
	}
	_ = sink
}
