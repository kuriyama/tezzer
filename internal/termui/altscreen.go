package termui

import "bytes"

// ScreenState is the terminal screen buffer inferred from the output stream.
type ScreenState int

const (
	// ScreenUnknown means the state cannot be inferred, e.g. the stream was
	// joined midway or has a gap.
	ScreenUnknown ScreenState = iota
	// ScreenMain is the normal screen buffer.
	ScreenMain
	// ScreenAlt is the alternate screen buffer used by full-screen apps.
	ScreenAlt
)

func (s ScreenState) String() string {
	switch s {
	case ScreenMain:
		return "main"
	case ScreenAlt:
		return "alt"
	default:
		return "unknown"
	}
}

// スキャナの内部状態
const (
	altGround  = iota // 通常テキスト（ESC を探すだけ）
	altEsc            // ESC の直後
	altCSI            // ESC [ の直後
	altPrivate        // ESC [ ? のパラメータ収集中
	altIgnore         // 無関係な CSI の残り（最終バイトまで読み飛ばす）
)

// maxParam はパラメータ値の上限（これ以上の桁は alt screen 番号にならないので打ち止め）
const maxParam = 100000

// AltScreenTracker infers whether the terminal is on the alternate screen by
// scanning output for DECSET/DECRST 1049, 1047 and 47 and for RIS (ESC c).
// Everything else is skipped. State carries over chunk boundaries, so escape
// sequences split across Scan calls are handled. It is not safe for
// concurrent use.
type AltScreenTracker struct {
	state   ScreenState
	scan    int  // altGround など
	param   int  // 収集中のパラメータ値
	hit     bool // 既出パラメータに alt screen 番号があった
	invalid bool // 中間バイト付き（DECSET/DECRST ではない）
}

// NewAltScreenTracker returns a tracker in the given initial state.
func NewAltScreenTracker(initial ScreenState) *AltScreenTracker {
	return &AltScreenTracker{state: initial}
}

// State returns the current inferred screen state.
func (t *AltScreenTracker) State() ScreenState { return t.state }

// Reset marks the state as unknown, e.g. after a gap in the output stream.
func (t *AltScreenTracker) Reset() {
	t.state = ScreenUnknown
	t.scan = altGround
}

func isAltParam(p int) bool { return p == 1049 || p == 1047 || p == 47 }

// Scan feeds output bytes to the tracker and returns the resulting state.
func (t *AltScreenTracker) Scan(p []byte) ScreenState {
	for i := 0; i < len(p); i++ {
		if t.scan == altGround {
			// 高速パス: ESC までまとめて読み飛ばす
			j := bytes.IndexByte(p[i:], 0x1b)
			if j < 0 {
				return t.state
			}
			i += j
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
			switch b {
			case '[':
				t.scan = altCSI
			case 'c':
				t.state = ScreenMain // RIS: 端末リセットで通常画面に戻る
				t.scan = altGround
			default:
				t.scan = altGround
			}
		case altCSI:
			if b == '?' {
				t.scan = altPrivate
				t.param, t.hit, t.invalid = 0, false, false
			} else {
				t.scan = altIgnore
				i-- // この b 自体が最終バイトの可能性があるので altIgnore で読み直す
			}
		case altPrivate:
			switch {
			case b >= '0' && b <= '9':
				if t.param < maxParam {
					t.param = t.param*10 + int(b-'0')
				}
			case b == ';' || b == ':':
				if isAltParam(t.param) {
					t.hit = true
				}
				t.param = 0
			case b >= 0x20 && b <= 0x2f:
				t.invalid = true
			case b >= 0x40 && b <= 0x7e:
				if !t.invalid && (t.hit || isAltParam(t.param)) {
					switch b {
					case 'h':
						t.state = ScreenAlt
					case 'l':
						t.state = ScreenMain
					}
				}
				t.scan = altGround
			default:
				// C0 制御文字（CSI 中でも実行されるだけで中断しない）等は無視
			}
		case altIgnore:
			if b >= 0x40 && b <= 0x7e {
				t.scan = altGround
			}
		}
	}
	return t.state
}
