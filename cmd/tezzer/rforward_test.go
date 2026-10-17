package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRemoteForwardSpec(t *testing.T) {
	tests := []struct {
		spec    string
		port    uint16
		target  string
		wantErr bool
	}{
		{spec: "9222:localhost:9222", port: 9222, target: "localhost:9222"},
		{spec: "localhost:9223:127.0.0.1:9222", port: 9223, target: "127.0.0.1:9222"},
		{spec: "[::1]:9222:[::1]:9222", port: 9222, target: "[::1]:9222"},
		{spec: "127.0.0.1:9222:host.internal:9222", port: 9222, target: "host.internal:9222"},
		{spec: "0.0.0.0:9222:localhost:9222", wantErr: true}, // loopback 以外の bind
		{spec: "0:localhost:9222", wantErr: true},            // 動的割り当ては非対応
		{spec: "9222:localhost", wantErr: true},
		{spec: "9222::9222", wantErr: true},
	}
	for _, tt := range tests {
		sp, err := parseRemoteForwardSpec(tt.spec)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %+v", tt.spec, sp)
			}
			continue
		}
		if err != nil || sp.port != tt.port || sp.target != tt.target {
			t.Errorf("%q: got %+v, %v; want port %d target %s", tt.spec, sp, err, tt.port, tt.target)
		}
	}
}

func TestParseRemoteForwardSpecsDuplicate(t *testing.T) {
	if _, err := parseRemoteForwardSpecs([]string{"9222:localhost:9222", "localhost:9222:localhost:9333"}); err == nil {
		t.Fatal("duplicate port accepted")
	}
}

func TestLoadOrCreateDeviceID(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".tezzer", "device-id")
	id, err := loadOrCreateDeviceID(path)
	if err != nil || !validDeviceID(id) {
		t.Fatalf("create: id=%q err=%v", id, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v mode=%v", err, fi.Mode())
	}
	again, err := loadOrCreateDeviceID(path)
	if err != nil || again != id {
		t.Fatalf("reload: %q, %v; want %q", again, err, id)
	}

	// 壊れた内容は作り直さない（O_EXCL で作れないので、このプロセス限りの ID と警告になる）。
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp, err := loadOrCreateDeviceID(path)
	if err == nil || !validDeviceID(tmp) {
		t.Fatalf("broken file: id=%q err=%v, want ephemeral id with warning", tmp, err)
	}
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != "garbage" {
		t.Fatal("broken file was overwritten")
	}
}

// 送る ID は secret とホスト名から決まる。共有ホーム（同じ secret）でもホスト名が
// 違えば別の端末になり、ホスト名も secret も含めて生のまま送らない。
func TestDeviceIDMixesHostname(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	a := deviceID(secret, "host-a")
	if a != deviceID(secret, "host-a") {
		t.Fatal("not stable")
	}
	if a == deviceID(secret, "host-b") {
		t.Fatal("same ID for different hostnames (shared home would be bundled)")
	}
	if a == deviceID("fedcba9876543210fedcba9876543210", "host-a") {
		t.Fatal("same ID for different secrets")
	}
	if !validDeviceID(a) || a == secret || strings.Contains(a, "host-a") {
		t.Fatalf("unexpected ID %q", a)
	}
}
