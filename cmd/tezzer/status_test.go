package main

import (
	"strings"
	"testing"

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
