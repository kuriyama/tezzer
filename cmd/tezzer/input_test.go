package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/kuriyama/tezzer/internal/termui"
)

// isCSIFinal/shouldFlushEscBuf は、UTF-8 境界分割（utf8IncompleteTrail、utf8_test.go）と
// 同じ「バッチ境界でシーケンスが分断されないか」というバグ種を CSI エスケープ
// シーケンス側で担う。UTF-8 側は手厚くテストされていたのに、こちらは1本もなかった。

func TestIsCSIFinal(t *testing.T) {
	tests := []struct {
		name string
		b    byte
		want bool
	}{
		{"tilde", '~', true},
		{"upper A", 'A', true},
		{"upper Z", 'Z', true},
		{"lower a", 'a', true},
		{"lower z", 'z', true},
		{"digit", '3', false},
		{"semicolon (param separator)", ';', false},
		{"ESC itself", 0x1b, false},
		{"CSI intro bracket", '[', false},
		{"space", ' ', false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCSIFinal(tt.b); got != tt.want {
				t.Errorf("isCSIFinal(%q) = %v, want %v", tt.b, got, tt.want)
			}
		})
	}
}

func TestShouldFlushEscBuf(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"empty", []byte{}, false},
		{"ESC only", []byte{0x1b}, false},
		{"ESC [ only (2 bytes)", []byte{0x1b, '['}, false},

		{"not starting with ESC", []byte{'a', 'b', 'c'}, false},

		// ESC O A/B/C/D: application cursor keys、即 flush。
		{"ESC O A (up)", []byte{0x1b, 'O', 'A'}, true},
		{"ESC O B (down)", []byte{0x1b, 'O', 'B'}, true},
		{"ESC O C (right)", []byte{0x1b, 'O', 'C'}, true},
		{"ESC O D (left)", []byte{0x1b, 'O', 'D'}, true},
		{"ESC O X (unknown)", []byte{0x1b, 'O', 'X'}, false},

		// ESC [ A/B/C/D: CSI cursor keys、ちょうど3バイトなら即 flush。
		{"ESC [ A (up)", []byte{0x1b, '[', 'A'}, true},
		{"ESC [ D (left)", []byte{0x1b, '[', 'D'}, true},

		// それ以外の CSI は final byte が来るまで待つ（早すぎる flush でシーケンスが
		// 分断されるとサーバー側で解釈できなくなる）。
		{"ESC [ 3 (Delete, waiting for final byte)", []byte{0x1b, '[', '3'}, false},
		{"ESC [ 3 ~ (Delete, complete)", []byte{0x1b, '[', '3', '~'}, true},
		{"ESC [ 1 5 (F5, waiting for final byte)", []byte{0x1b, '[', '1', '5'}, false},
		{"ESC [ 1 5 ~ (F5, complete)", []byte{0x1b, '[', '1', '5', '~'}, true},
		{"ESC [ 2 0 0 (bracketed paste start, waiting)", []byte{0x1b, '[', '2', '0', '0'}, false},

		// 32バイトに達したら内容によらず強制 flush（無限に溜め込まない安全弁）。
		{"32 bytes forces flush even mid-sequence", append([]byte{0x1b, '[', '9'}, make([]byte, 29)...), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldFlushEscBuf(tt.buf); got != tt.want {
				t.Errorf("shouldFlushEscBuf(%q) = %v, want %v", tt.buf, got, tt.want)
			}
		})
	}
}

// TestNoteSendResult は接続断中の入力送信エラーが 1 行に抑えられ、復帰時に破棄件数が
// 1 行だけ出ることを確認する（2026-09 報告: 断中にマウスの ESC シーケンスで大量ログ）。
func TestNoteSendResult(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	c := &Client{}
	errDead := errors.New("timeout: no recent network activity")

	c.noteSendResult("ESC batch", 3, nil)
	if buf.Len() != 0 {
		t.Fatalf("success while healthy should not log, got %q", buf.String())
	}

	for i := 0; i < 50; i++ {
		c.noteSendResult("ESC batch", 6, errDead)
	}
	c.noteSendResult("Input batch", 4, errDead)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "ESC batch send error") {
		t.Fatalf("want exactly 1 error line, got %q", buf.String())
	}

	buf.Reset()
	c.noteSendResult("Input batch", 1, nil)
	if !strings.Contains(buf.String(), "51 input chunk(s) (304 bytes) dropped") {
		t.Fatalf("recovery line missing drop counts: %q", buf.String())
	}

	// 復帰後は再び最初の失敗を 1 行出す
	buf.Reset()
	c.noteSendResult("ESC batch", 3, errDead)
	if !strings.Contains(buf.String(), "send error") {
		t.Fatalf("error after recovery should log again, got %q", buf.String())
	}
}

// TestLogQUICSetupFailure は QUIC 確立失敗の全文が stderr ログに出て、同じ内容の
// 再試行失敗では繰り返し出ないことを確認する（status line だけだと理由が切れて読めない）。
func TestLogQUICSetupFailure(t *testing.T) {
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	c := &Client{statusMgr: termui.NewStatusManager(func(string) {}, func() {})}
	errA := errors.New("failed to connect: all 1 candidate(s) failed: 192.0.2.1:48175: timeout: no recent network activity")

	last := c.logQUICSetupFailure(errA, "", 48175)
	last = c.logQUICSetupFailure(errA, last, 48175)
	if n := strings.Count(buf.String(), "QUIC setup failed"); n != 1 {
		t.Fatalf("same error should be logged once, got %d: %q", n, buf.String())
	}
	if !strings.Contains(buf.String(), "192.0.2.1:48175") {
		t.Fatalf("full error text missing: %q", buf.String())
	}
	if n := strings.Count(buf.String(), "hint: no response from UDP port 48175"); n != 1 {
		t.Fatalf("timeout failure should add the hint once, got %d: %q", n, buf.String())
	}

	c.logQUICSetupFailure(errors.New("different"), last, 48175)
	if n := strings.Count(buf.String(), "QUIC setup failed"); n != 2 {
		t.Fatalf("changed error should be logged again, got %d", n)
	}
}

func TestQUICSetupHint(t *testing.T) {
	if h := quicSetupHint("all 1 candidate(s) failed: 192.0.2.1:7020: timeout: no recent network activity", 7020); !strings.Contains(h, "UDP port 7020") {
		t.Errorf("idle timeout should give hint, got %q", h)
	}
	if h := quicSetupHint("x: timeout: handshake did not complete in time", 7020); h == "" {
		t.Error("handshake timeout should give hint")
	}
	if h := quicSetupHint("x: CRYPTO_ERROR 0x12a (remote): tls: bad certificate", 7020); h != "" {
		t.Errorf("non-timeout failure should not give hint, got %q", h)
	}
}
