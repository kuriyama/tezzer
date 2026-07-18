package main

import (
	"reflect"
	"testing"
)

func TestBuildUDPCandidateAddrs(t *testing.T) {
	tests := []struct {
		name            string
		quicInfo        *QUICInfo
		clientSTUNAddrs []string
		wantCandidates  []string
	}{
		{
			name: "same host via matching v4 STUN address",
			quicInfo: &QUICInfo{
				Port:      12345,
				LocalAddr: "10.0.0.5:12345",
				STUNAddrs: []string{"203.0.113.1:12345"},
			},
			clientSTUNAddrs: []string{"203.0.113.1:54321"},
			wantCandidates:  []string{"10.0.0.5:12345", "203.0.113.1:12345"},
		},
		{
			name: "remote: no matching family",
			quicInfo: &QUICInfo{
				Port:      12345,
				LocalAddr: "10.0.0.5:12345",
				STUNAddrs: []string{"203.0.113.1:12345"},
			},
			clientSTUNAddrs: []string{"198.51.100.9:54321"},
			wantCandidates:  []string{"203.0.113.1:12345"},
		},
		{
			name: "v4 differs but v6 matches: still same host",
			quicInfo: &QUICInfo{
				Port:      12345,
				LocalAddr: "10.0.0.5:12345",
				STUNAddrs: []string{"203.0.113.1:12345", "[2001:db8::1]:12345"},
			},
			clientSTUNAddrs: []string{"198.51.100.9:54321", "[2001:db8::1]:9999"},
			wantCandidates: []string{
				"10.0.0.5:12345",
				"203.0.113.1:12345",
				"[2001:db8::1]:12345",
			},
		},
		{
			name: "no STUN data on either side: treated as same host",
			quicInfo: &QUICInfo{
				Port:      12345,
				LocalAddr: "10.0.0.5:12345",
			},
			clientSTUNAddrs: nil,
			wantCandidates:  []string{"10.0.0.5:12345"},
		},
		{
			name:            "no candidates at all: falls back to loopback",
			quicInfo:        &QUICInfo{Port: 12345},
			clientSTUNAddrs: nil,
			wantCandidates:  []string{"127.0.0.1:12345"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildQUICCandidateAddrs(tt.quicInfo, tt.clientSTUNAddrs)
			if !reflect.DeepEqual(got, tt.wantCandidates) {
				t.Errorf("buildQUICCandidateAddrs() = %v, want %v", got, tt.wantCandidates)
			}
		})
	}
}
