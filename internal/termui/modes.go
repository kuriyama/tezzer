package termui

import (
	"bytes"
	"strconv"
)

// trackedMode は ModeTracker が追跡する DECSET/DECRST の private mode。
type trackedMode struct {
	num int
	def bool // 端末の初期状態（RIS 後の値）
}

// trackedModes は追跡対象のモード。Restore はこの順で出力するため、alt screen を
// 先頭に置く（alt screen へ入ってから他のモードを立てる）。
// 選定は「アプリが起動時に 1 回だけ有効化し、attach し直すと失われると困るもの」で、
// クライアントが detach 時に解除するもの（cmd/tezzer の terminalModeReset）と揃える。
var trackedModes = []trackedMode{
	{47, false}, {1047, false}, {1049, false}, // alt screen
	{1000, false}, {1002, false}, {1003, false}, // マウス通知
	{1005, false}, {1006, false}, {1015, false}, // マウス座標のエンコーディング
	{1004, false}, // フォーカス通知
	{2004, false}, // bracketed paste
	{1, false},    // application cursor keys (DECCKM)
	{25, true},    // カーソル表示
}

// keypadBit は application keypad（ESC = / ESC >）のビット位置（trackedModes の後ろ）。
var keypadBit = uint(len(trackedModes))

// maxModeParams は 1 つの DECSET/DECRST で受け付けるパラメータ数の上限（超過分は無視）。
const maxModeParams = 16

// ModeTracker tracks the terminal modes that full-screen apps usually enable
// once at startup (mouse reporting, bracketed paste, focus events, the
// alternate screen, cursor/keypad modes and cursor visibility), by scanning
// output for DECSET/DECRST, DECKPAM/DECKPNM and RIS. Restore returns the
// escape sequence that brings a terminal in its initial state to the tracked
// state, so a client that attaches later can be given the modes the app set
// long ago. State carries over chunk boundaries. The zero value is a tracker
// with every mode at its initial value. It is not safe for concurrent use.
type ModeTracker struct {
	diff    uint32 // bit i: trackedModes[i]（keypadBit: keypad）が初期値と異なる
	scan    int    // altGround など（AltScreenTracker と共通の状態）
	params  [maxModeParams]int
	nparams int
	invalid bool // 中間バイト付き（DECSET/DECRST ではない）
}

// Restore returns the escape sequence that reproduces the tracked modes on a
// terminal in its initial state, or nil when every mode is at its initial value.
func (t *ModeTracker) Restore() []byte {
	if t.diff == 0 {
		return nil
	}
	var b bytes.Buffer
	for i, m := range trackedModes {
		if t.diff&(1<<uint(i)) == 0 {
			continue
		}
		b.WriteString("\x1b[?")
		b.WriteString(strconv.Itoa(m.num))
		if m.def {
			b.WriteByte('l')
		} else {
			b.WriteByte('h')
		}
	}
	if t.diff&(1<<keypadBit) != 0 {
		b.WriteString("\x1b=")
	}
	return b.Bytes()
}

func (t *ModeTracker) setMode(num int, on bool) {
	for i, m := range trackedModes {
		if m.num == num {
			t.setBit(uint(i), on != m.def)
			return
		}
	}
}

func (t *ModeTracker) setBit(bit uint, differs bool) {
	if differs {
		t.diff |= 1 << bit
	} else {
		t.diff &^= 1 << bit
	}
}

// Scan feeds output bytes to the tracker.
func (t *ModeTracker) Scan(p []byte) {
	for i := 0; i < len(p); i++ {
		if t.scan == altGround {
			// 高速パス: ESC までまとめて読み飛ばす
			j := bytes.IndexByte(p[i:], 0x1b)
			if j < 0 {
				return
			}
			i += j
			// TUI の出力は ESC [ <非 ?>（SGR・カーソル移動）が大半なので、状態機械を
			// 経由せずその場で捨てて次の ESC を探す（ESC [ と次の 1 バイトを読み飛ばす）。
			if i+2 < len(p) && p[i+1] == '[' && p[i+2] != '?' && p[i+2] != 0x1b && p[i+2] != 0x18 && p[i+2] != 0x1a {
				i += 2
				continue
			}
			t.scan = altEsc
			continue
		}
		b := p[i]
		if b == 0x1b {
			// シーケンス途中の ESC は新しいシーケンスの開始として扱う
			t.scan = altEsc
			continue
		}
		if b == 0x18 || b == 0x1a {
			// CAN / SUB はシーケンスを中断する
			t.scan = altGround
			continue
		}
		switch t.scan {
		case altEsc:
			t.scan = altGround
			switch b {
			case '[':
				t.scan = altCSI
			case 'c':
				t.diff = 0 // RIS: 端末リセットで全モードが初期値に戻る
			case '=':
				t.setBit(keypadBit, true) // DECKPAM
			case '>':
				t.setBit(keypadBit, false) // DECKPNM
			}
		case altCSI:
			if b == '?' {
				t.scan = altPrivate
				t.params[0], t.nparams, t.invalid = 0, 1, false
			} else {
				// 追跡対象外の CSI（SGR・カーソル移動等。TUI の出力の大半）。最終バイトまで
				// 1 バイトずつ読み飛ばす必要はない: CSI の中に ESC は現れず（現れたら新しい
				// シーケンスの開始）、対象外の CSI の中身は状態に影響しないので、次の ESC を
				// 探す高速パスへ戻るのと結果は同じになる。
				t.scan = altGround
			}
		case altPrivate:
			switch {
			case b >= '0' && b <= '9':
				if p := &t.params[t.nparams-1]; *p < maxParam {
					*p = *p*10 + int(b-'0')
				}
			case b == ';' || b == ':':
				if t.nparams < maxModeParams {
					t.params[t.nparams] = 0
					t.nparams++
				} else {
					t.params[t.nparams-1] = 0 // 上限超過分は捨てる（最後の枠を使い回す）
				}
			case b >= 0x20 && b <= 0x2f:
				t.invalid = true
			case b >= 0x40 && b <= 0x7e:
				if !t.invalid && (b == 'h' || b == 'l') {
					for _, n := range t.params[:t.nparams] {
						t.setMode(n, b == 'h')
					}
				}
				t.scan = altGround
			default:
				// C0 制御文字（CSI 中でも実行されるだけで中断しない）等は無視
			}
		}
	}
}
