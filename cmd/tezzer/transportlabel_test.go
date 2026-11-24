package main

import (
	"testing"

	"github.com/kuriyama/tezzer/internal/qtransport"
)

// TestTransportLabel は Ctrl-^ i の経路表示（先頭 2 文字が UD / Q4 / Q6 の固定カラム）を検査する。
func TestTransportLabel(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"uds only", udsLabel(quicSetupNone), "UD"},
		{"uds connecting", udsLabel(quicSetupConnecting), "UD connecting QUIC"},
		{"uds retrying", udsLabel(quicSetupRetrying), "UD retrying QUIC"},
		{"quic v4", quicLabel("203.0.113.1:4242", qtransport.StateConnected, 12.4, "▁▂"), "Q4 RTT:12ms ▁▂"},
		{"quic v6", quicLabel("[2001:db8::1]:4242", qtransport.StateConnected, 15, ""), "Q6 RTT:15ms "},
		{"quic v4-mapped v6", quicLabel("[::ffff:192.0.2.1]:4242", qtransport.StateConnected, 1, ""), "Q4 RTT:1ms "},
		{"quic recovering", quicLabel("203.0.113.1:4242", qtransport.StateRecovering, 12, "▁"), "Q4 recovering"},
		{"quic closed", quicLabel("[2001:db8::1]:4242", qtransport.StateClosed, 0, ""), "Q6 closed"},
		{"unknown family", quicLabel("", qtransport.StateConnected, 3, ""), "Q? RTT:3ms "},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}
