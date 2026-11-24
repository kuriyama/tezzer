package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/qtransport"
	"github.com/kuriyama/tezzer/internal/termui"
)

func TestStatusOffSuppressesUnsolicited(t *testing.T) {
	var shown []string
	newClient := func(off bool) *Client {
		shown = nil
		c := &Client{statusOff: off}
		c.statusMgr = termui.NewStatusManager(func(msg string) { shown = append(shown, msg) }, func() {})
		t.Cleanup(c.statusMgr.Close)
		return c
	}

	c := newClient(true)
	c.setStatusMessage("[QUIC] connected via x")
	if len(shown) != 0 {
		t.Errorf("off: unsolicited message was displayed: %q", shown)
	}
	if logs := c.getLogMessages(); len(logs) != 1 || logs[0].message != "[QUIC] connected via x" {
		t.Errorf("off: unsolicited message not logged: %+v", logs)
	}
	c.showRequestedStatus("[Tezzer] sess:abc")
	if len(shown) != 1 || shown[0] != "[Tezzer] sess:abc" {
		t.Errorf("off: requested message not displayed: %q", shown)
	}

	c = newClient(false)
	c.setStatusMessage("[QUIC] connected via x")
	if len(shown) != 1 {
		t.Errorf("default: unsolicited message not displayed: %q", shown)
	}
}

func TestNameLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"A-backend", "name:A-backend "},
		{"exactly-twenty-chars", "name:exactly-twenty-chars "},
		{"a-very-long-session-name-over-20", "name:a-very-long-session~ "},
	}
	for _, tt := range tests {
		if got := nameLabel(tt.in); got != tt.want {
			t.Errorf("nameLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShowStatusIncludesName(t *testing.T) {
	var shown []string
	c := &Client{sessionID: "avHB52L2d4MmTJa", sessionName: "A-backend"}
	c.statusMgr = termui.NewStatusManager(func(msg string) { shown = append(shown, msg) }, func() {})
	t.Cleanup(c.statusMgr.Close)
	c.showStatus()
	if len(shown) != 1 || !strings.HasPrefix(shown[0], "[Tezzer] name:A-backend sess:avHB52 | ") {
		t.Errorf("showStatus = %q, want prefix %q", shown, "[Tezzer] name:A-backend sess:avHB52 | ")
	}
}

func TestFitStatusWidth(t *testing.T) {
	tests := []struct {
		msg   string
		width int
		want  string
	}{
		{"[Tezzer] sess:abc", 0, "[Tezzer] sess:abc"},
		{"[Tezzer] sess:abc", 17, "[Tezzer] sess:abc"},
		{"[Tezzer] sess:abc", 10, "[Tezzer] ~"},
		// ブロック文字は曖昧幅なので 2 桁と数える
		{"RTT:1ms ▁▂▃▅▂", 10, "RTT:1ms ~"},
		{"RTT:1ms ▁▂", 12, "RTT:1ms ▁▂"},
		{"RTT:1ms ▁▂", 11, "RTT:1ms ▁~"},
		{"abc", 1, "~"},
	}
	for _, tt := range tests {
		if got := fitStatusWidth(tt.msg, tt.width); got != tt.want {
			t.Errorf("fitStatusWidth(%q, %d) = %q, want %q", tt.msg, tt.width, got, tt.want)
		}
	}
}

func TestPortsLabel(t *testing.T) {
	tests := []struct {
		ports []string
		want  string
	}{
		{nil, ""},
		{[]string{"8080"}, "[8080]"},
		{[]string{"8080", "5432", "3000"}, "[8080,5432,3000]"},
		{[]string{"8080", "5432", "3000", "9222", "6379"}, "[8080,5432,3000+2]"},
	}
	for _, tt := range tests {
		if got := portsLabel(tt.ports); got != tt.want {
			t.Errorf("portsLabel(%q) = %q, want %q", tt.ports, got, tt.want)
		}
	}
}

func TestShowStatusIncludesForwardPorts(t *testing.T) {
	var shown []string
	c := &Client{
		sessionID: "avHB52L2d4MmTJa",
		forwards: []forwardSpec{
			{listenAddr: "localhost:8080", target: "localhost:3000"},
			{listenAddr: "[::1]:5432", target: "db:5432"},
		},
		rforwards: []remoteForwardSpec{{port: 9222, target: "localhost:9222"}},
	}
	c.statusMgr = termui.NewStatusManager(func(msg string) { shown = append(shown, msg) }, func() {})
	t.Cleanup(c.statusMgr.Close)
	c.showStatus()
	if len(shown) != 1 || !strings.HasSuffix(shown[0], " | fwd:0/2[8080,5432] | rfwd:0/1[9222]") {
		t.Errorf("showStatus = %q, want suffix %q", shown, " | fwd:0/2[8080,5432] | rfwd:0/1[9222]")
	}
}

// 終了処理が始まった後の setTransport は設定せずに transport を閉じる（QUIC の再試行が終了と
// 重なったとき、誰にも閉じられない接続を残さない）。
func TestSetTransportAfterFinish(t *testing.T) {
	c := &Client{done: make(chan struct{})}
	c.finish(nil)
	ct := qtransport.NewClient(qtransport.ClientConfig{Key: make([]byte, 32), Candidates: []string{"192.0.2.1:4242"}})
	if c.setTransport(ct) {
		t.Fatal("setTransport after finish returned true")
	}
	if c.transport() != nil {
		t.Error("transport set after finish")
	}
	if ct.State() != qtransport.StateClosed {
		t.Errorf("transport state = %v, want StateClosed", ct.State())
	}
}

// waitRendered はセッション終了の通知が出力を追い越したとき、最後の出力が描画されるまで待つ。
func TestWaitRendered(t *testing.T) {
	c := &Client{done: make(chan struct{})}
	c.renderedSeq.Store(3)
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.renderedSeq.Store(5)
	}()
	start := time.Now()
	c.waitRendered(5)
	if got := c.renderedSeq.Load(); got != 5 {
		t.Errorf("waitRendered returned before the final output was rendered (renderedSeq=%d)", got)
	}
	if d := time.Since(start); d >= finalOutputWait {
		t.Errorf("waitRendered took %v, want it to return once rendered", d)
	}
	// 旧サーバ（finalSeq 不明）は待たない
	start = time.Now()
	c.waitRendered(0)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("waitRendered(0) took %v, want no wait", d)
	}
}
