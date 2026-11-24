package main

// lostinput.go: 届かなかった入力の表示（qtransport の OnInputLost）。
// 経路断の前後に打った入力のうち、セッションに届かなかったものを復帰時に知らせる。
// 複数ウィンドウで作業しているときに「どこで何を打って詰まったか」を思い出すきっかけ用。
// 内容にはパスワードが混ざりうるので、自動通知（ステータス行・ログファイル）はバイト数
// だけにし、冒頭はユーザーが Ctrl-^ i を押したときに画面にだけ出す（ログには残さない）。

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// lostPreviewRunes は Ctrl-^ i に出す届かなかった入力の冒頭の文字数。
const lostPreviewRunes = 16

// lostShowFor は届かなかった入力を Ctrl-^ i に出し続ける時間。
const lostShowFor = 10 * time.Minute

type lostInputInfo struct {
	total   int
	preview string
	at      time.Time
}

// onInputLost は qtransport の OnInputLost。内容はここで冒頭の表示用文字列にしてだけ残す。
func (c *Client) onInputLost(total int, head []byte) {
	c.lostMu.Lock()
	c.lastLost = lostInputInfo{total: total, preview: lostInputPreview(head, total > len(head)), at: time.Now()}
	c.lostMu.Unlock()
	c.setStatusMessage(fmt.Sprintf("[Tezzer] %d bytes of input were not delivered (%s i to see the beginning)",
		total, escapeKeyDisplay(c.escapeByte)))
}

// lostInputLabel は Ctrl-^ i に入れる `lost:"make dep…"(23B) | `（直近 lostShowFor 以内に
// なければ空）。
func (c *Client) lostInputLabel() string {
	c.lostMu.Lock()
	info := c.lastLost
	c.lostMu.Unlock()
	if info.total == 0 || time.Since(info.at) > lostShowFor {
		return ""
	}
	return fmt.Sprintf("lost:\"%s\"(%dB) | ", info.preview, info.total)
}

// terminalReports は端末が自動で送る入力（マウス報告・フォーカス出入り）。打った文字が
// 埋もれないよう表示から省く。
var terminalReports = regexp.MustCompile(`\x1b\[<\d+;\d+;\d+[Mm]|\x1b\[M[\s\S]{3}|\x1b\[[IO]`)

// lostInputPreview は届かなかった入力の冒頭を 1 行で読める形にする。Enter は ⏎、Tab は ⇥、
// その他の制御文字は ^C のようにし、lostPreviewRunes 文字を超える（または more）なら … を付ける。
func lostInputPreview(head []byte, more bool) string {
	b := terminalReports.ReplaceAll(head, nil)
	var sb strings.Builder
	n := 0
	for len(b) > 0 {
		if n >= lostPreviewRunes {
			more = true
			break
		}
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		switch {
		case r == '\r' || r == '\n':
			sb.WriteRune('⏎')
		case r == '\t':
			sb.WriteRune('⇥')
		case r == 0x7f:
			sb.WriteString("^?")
		case r < 0x20:
			sb.WriteByte('^')
			sb.WriteByte(byte(r) + 0x40)
		case r == utf8.RuneError && size == 1:
			sb.WriteRune('?')
		default:
			sb.WriteRune(r)
		}
		n++
	}
	if more {
		sb.WriteRune('…')
	}
	return sb.String()
}
