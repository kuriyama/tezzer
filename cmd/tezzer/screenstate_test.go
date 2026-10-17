package main

import (
	"testing"

	"github.com/kuriyama/tezzer/internal/termui"
	"github.com/kuriyama/tezzer/internal/transport"
)

func newScreenTestClient() *Client {
	return &Client{
		noPTY:        true, // writeOutput が stdout に書かないように
		statusRedraw: true,
		altScreen:    termui.NewAltScreenTracker(termui.ScreenUnknown),
	}
}

func chunk(off uint64, s string) []transport.OutputChunk {
	return []transport.OutputChunk{{Offset: off, Data: []byte(s)}}
}

func (c *Client) screen() termui.ScreenState { return termui.ScreenState(c.screenState.Load()) }

func TestTrackScreenFromSessionStart(t *testing.T) {
	c := newScreenTestClient()
	// offset 1 から見ていれば遷移なしでも通常画面で確定する
	c.renderOutput(chunk(1, "$ "), true)
	if got := c.screen(); got != termui.ScreenMain {
		t.Fatalf("after offset 1 = %v, want main", got)
	}
	c.renderOutput(chunk(2, "\x1b[?1049h"), true)
	if got := c.screen(); got != termui.ScreenAlt {
		t.Fatalf("after enter = %v, want alt", got)
	}
}

func TestTrackScreenJoinMidway(t *testing.T) {
	c := newScreenTestClient()
	// 途中参加（先頭が evict 済み）は遷移を見るまで不明のまま
	c.renderOutput(chunk(100, "\x1b[2J\x1b[Hredraw"), true)
	if got := c.screen(); got != termui.ScreenUnknown {
		t.Fatalf("join midway = %v, want unknown", got)
	}
	c.renderOutput(chunk(101, "\x1b[?1049l"), true)
	if got := c.screen(); got != termui.ScreenMain {
		t.Fatalf("after exit = %v, want main", got)
	}
}

func TestTrackScreenGapResets(t *testing.T) {
	c := newScreenTestClient()
	c.renderOutput(chunk(1, "\x1b[?1049h"), true)
	// QUIC 再同期で evict 分を飛ばした（間に 1049l があったかもしれない）
	c.renderOutput(chunk(50, "text"), true)
	if got := c.screen(); got != termui.ScreenUnknown {
		t.Fatalf("after gap = %v, want unknown", got)
	}
}

func TestTrackScreenSplitAcrossChunks(t *testing.T) {
	c := newScreenTestClient()
	c.renderOutput(chunk(1, "abc\x1b[?10"), true)
	c.renderOutput(chunk(2, "49hdef"), true)
	if got := c.screen(); got != termui.ScreenAlt {
		t.Fatalf("split sequence = %v, want alt", got)
	}
}

func TestTrackScreenDuplicateNotRescanned(t *testing.T) {
	c := newScreenTestClient()
	c.renderOutput(chunk(1, "\x1b[?1049h"), true)
	c.renderOutput(chunk(2, "\x1b[?1049l"), true)
	// 他経路からの再送（描画済み offset）は状態に影響しない
	c.renderOutput(chunk(1, "\x1b[?1049h"), false)
	if got := c.screen(); got != termui.ScreenMain {
		t.Fatalf("after duplicate = %v, want main", got)
	}
}

func TestTrackScreenDisabled(t *testing.T) {
	c := &Client{noPTY: true}
	c.renderOutput(chunk(1, "\x1b[?1049h"), true)
	if got := c.screen(); got != termui.ScreenUnknown {
		t.Fatalf("disabled = %v, want unknown (no tracking)", got)
	}
}

func TestShouldRedrawAfterStatus(t *testing.T) {
	// 無効・状態不明・通常画面・-peek では再描画しない
	cases := []struct {
		name  string
		setup func(c *Client)
	}{
		{"disabled", func(c *Client) { c.statusRedraw = false; c.screenState.Store(int32(termui.ScreenAlt)) }},
		{"unknown", func(c *Client) {}},
		{"main", func(c *Client) { c.screenState.Store(int32(termui.ScreenMain)) }},
		{"peek", func(c *Client) { c.readOnly = true; c.screenState.Store(int32(termui.ScreenAlt)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newScreenTestClient()
			tc.setup(c)
			if c.shouldRedrawAfterStatus() {
				t.Errorf("shouldRedrawAfterStatus() = true, want false")
			}
		})
	}
	c := newScreenTestClient()
	c.screenState.Store(int32(termui.ScreenAlt))
	if !c.shouldRedrawAfterStatus() {
		t.Errorf("alt screen: shouldRedrawAfterStatus() = false, want true")
	}
}

func TestRenderOutputStopsAfterRestoreTerminalModes(t *testing.T) {
	c := newScreenTestClient()
	c.renderOutput(chunk(1, "\x1b[?1000h"), true)
	c.restoreTerminalModes()
	c.restoreTerminalModes() // 2 回目は何もしない
	// 解除後に遅れて届いたチャンクは描画しない（モードを再び有効化しないように）
	c.renderOutput(chunk(2, "\x1b[?1000h"), true)
	if got := c.renderedSeq.Load(); got != 1 {
		t.Fatalf("renderedSeq = %d, want 1 (output after restore must be dropped)", got)
	}
}
