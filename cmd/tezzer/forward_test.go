package main

// forward_test.go: -L 指定のパース（parseForwardSpec）のテスト。
// bind の loopback 限定（設計方針）を含めて検証する。

import "testing"

func TestParseForwardSpec(t *testing.T) {
	cases := []struct {
		spec       string
		wantListen string
		wantTarget string
		wantErr    bool
	}{
		{"8080:localhost:3000", "127.0.0.1:8080", "localhost:3000", false},
		{"127.0.0.1:8080:localhost:3000", "127.0.0.1:8080", "localhost:3000", false},
		{"localhost:8080:db.internal:5432", "localhost:8080", "db.internal:5432", false},
		{"::1:invalid", "", "", true}, // 区切り不足
		{"[::1]:8080:localhost:3000", "[::1]:8080", "localhost:3000", false},
		{"8080:[::1]:3000", "127.0.0.1:8080", "[::1]:3000", false},
		// loopback 以外の bind は拒否（GatewayPorts 相当は作らない）
		{"0.0.0.0:8080:localhost:3000", "", "", true},
		{"192.168.1.1:8080:localhost:3000", "", "", true},
		{"example.com:8080:localhost:3000", "", "", true},
		// ポート不正
		{"0:localhost:3000", "", "", true},
		{"99999:localhost:3000", "", "", true},
		{"8080:localhost:http", "", "", true},
		// 形式不正
		{"8080", "", "", true},
		{"8080:localhost", "", "", true},
		{"a:b:c:d:e", "", "", true},
		{"8080::3000", "", "", true}, // host 空
	}
	for _, c := range cases {
		got, err := parseForwardSpec(c.spec)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseForwardSpec(%q) = %+v, want error", c.spec, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseForwardSpec(%q): %v", c.spec, err)
			continue
		}
		if got.listenAddr != c.wantListen || got.target != c.wantTarget {
			t.Errorf("parseForwardSpec(%q) = {%s, %s}, want {%s, %s}",
				c.spec, got.listenAddr, got.target, c.wantListen, c.wantTarget)
		}
	}
}
