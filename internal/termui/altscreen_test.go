package termui

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestAltScreenTracker(t *testing.T) {
	tests := []struct {
		name    string
		initial ScreenState
		in      string
		want    ScreenState
	}{
		{"plain text keeps state", ScreenMain, "hello\r\nworld", ScreenMain},
		{"1049 enter", ScreenMain, "\x1b[?1049h", ScreenAlt},
		{"1049 exit", ScreenAlt, "\x1b[?1049l", ScreenMain},
		{"1047 enter", ScreenMain, "\x1b[?1047h", ScreenAlt},
		{"47 enter", ScreenMain, "\x1b[?47h", ScreenAlt},
		{"unknown resolves on enter", ScreenUnknown, "x\x1b[?1049hy", ScreenAlt},
		{"unknown resolves on exit", ScreenUnknown, "\x1b[?1049l", ScreenMain},
		{"unknown stays without transition", ScreenUnknown, "\x1b[2J\x1b[H\x1b[1;31mX\x1b[0m", ScreenUnknown},
		{"multi param first", ScreenMain, "\x1b[?1049;1h", ScreenAlt},
		{"multi param last", ScreenMain, "\x1b[?1;1049h", ScreenAlt},
		{"unrelated private mode", ScreenMain, "\x1b[?25l\x1b[?1h\x1b[?2004h", ScreenMain},
		{"1048 is cursor save only", ScreenMain, "\x1b[?1048h", ScreenMain},
		{"10490 is not 1049", ScreenMain, "\x1b[?10490h", ScreenMain},
		{"intermediate byte is not DECSET", ScreenMain, "\x1b[?1049$p", ScreenMain},
		{"non-private CSI ignored", ScreenMain, "\x1b[1049h", ScreenMain},
		{"RIS returns to main", ScreenAlt, "\x1bc", ScreenMain},
		{"last transition wins", ScreenMain, "\x1b[?1049h...\x1b[?1049l..\x1b[?1049h", ScreenAlt},
		{"ESC restarts sequence", ScreenMain, "\x1b[?10\x1b[?1049h", ScreenAlt},
		{"CAN aborts sequence", ScreenMain, "\x1b[?1049\x18h", ScreenMain},
		{"OSC then enter", ScreenMain, "\x1b]0;title\x07\x1b[?1049h", ScreenAlt},
		{"huge param does not overflow", ScreenMain, "\x1b[?99999999999999999999999h", ScreenMain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewAltScreenTracker(tt.initial)
			if got := tr.Scan([]byte(tt.in)); got != tt.want {
				t.Errorf("Scan(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// どこで分割して渡しても結果が一括スキャンと一致すること（チャンク境界跨ぎ）
func TestAltScreenTrackerSplit(t *testing.T) {
	inputs := []string{
		"ab\x1b[?1049hcd",
		"\x1b[?1;1049h",
		"\x1b[?1049l\x1b[2J",
		"\x1b[?1049h\x1bc",
		"\x1b[?1049$p",
	}
	for _, in := range inputs {
		whole := NewAltScreenTracker(ScreenMain).Scan([]byte(in))
		for cut := 0; cut <= len(in); cut++ {
			tr := NewAltScreenTracker(ScreenMain)
			tr.Scan([]byte(in[:cut]))
			if got := tr.Scan([]byte(in[cut:])); got != whole {
				t.Errorf("%q split at %d = %v, want %v", in, cut, got, whole)
			}
		}
		// 1 バイトずつ
		tr := NewAltScreenTracker(ScreenMain)
		for i := 0; i < len(in); i++ {
			tr.Scan([]byte{in[i]})
		}
		if got := tr.State(); got != whole {
			t.Errorf("%q byte-by-byte = %v, want %v", in, got, whole)
		}
	}
}

func TestAltScreenTrackerReset(t *testing.T) {
	tr := NewAltScreenTracker(ScreenMain)
	tr.Scan([]byte("\x1b[?1049h\x1b[?10"))
	tr.Reset()
	if tr.State() != ScreenUnknown {
		t.Fatalf("after Reset = %v, want unknown", tr.State())
	}
	// Reset でシーケンス途中の状態も捨てる（"49h" を続きとして解釈しない）
	if got := tr.Scan([]byte("49h")); got != ScreenUnknown {
		t.Errorf("after Reset + tail = %v, want unknown", got)
	}
}

// 典型的な TUI 出力（SGR・カーソル移動が多い）を模した 1MB
func benchOutput() []byte {
	r := rand.New(rand.NewSource(1))
	var b bytes.Buffer
	for b.Len() < 1<<20 {
		b.WriteString("\x1b[")
		b.WriteString([]string{"1;31m", "0m", "38;5;255m", "12;40H", "K", "?25l"}[r.Intn(6)])
		n := r.Intn(40)
		for i := 0; i < n; i++ {
			b.WriteByte(byte('a' + r.Intn(26)))
		}
	}
	return b.Bytes()
}

func BenchmarkAltScreenTrackerTUI(b *testing.B) {
	data := benchOutput()
	tr := NewAltScreenTracker(ScreenMain)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Scan(data)
	}
}

func BenchmarkAltScreenTrackerPlain(b *testing.B) {
	data := bytes.Repeat([]byte("plain log line without escapes 0123456789\r\n"), 1<<20/44)
	tr := NewAltScreenTracker(ScreenMain)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Scan(data)
	}
}
