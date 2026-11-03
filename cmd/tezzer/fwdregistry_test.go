package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardRegistryRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &Client{
		sessionID:   "avHB52L2d4MmTJa",
		sessionName: "work",
		forwards: []forwardSpec{
			{listenAddr: "localhost:8080", target: "localhost:3000"},
			{listenAddr: "localhost:5432", target: "db:5432"},
		},
		rforwards: []remoteForwardSpec{{port: 9222, target: "localhost:9222"}},
	}
	entry := newForwardRegistryEntry(c, "myserver", []error{nil, errors.New("address already in use")})
	unregister, err := registerForwards(entry)
	if err != nil {
		t.Fatalf("registerForwards: %v", err)
	}

	entries, err := readForwardRegistry()
	if err != nil {
		t.Fatalf("readForwardRegistry: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	got := formatForwardRegistry(entries)
	for _, want := range []string{
		"myserver", "work (avHB52)",
		"localhost:8080", "localhost:3000", "listening",
		"bind failed: address already in use",
		"server:9222", "requested (see tezzer -list)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}

	unregister()
	entries, err = readForwardRegistry()
	if err != nil {
		t.Fatalf("readForwardRegistry after unregister: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries after unregister = %+v, want none", entries)
	}
}

// ロックを持つプロセスがいないファイル（異常終了の残骸）は読み飛ばして消す
func TestForwardRegistryRemovesStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := fwdRegistryDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "99999.json")
	if err := os.WriteFile(stale, []byte(`{"pid":99999,"server":"x","session_id":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := readForwardRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("stale entry was listed: %+v", entries)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file not removed: %v", err)
	}
}

// 残骸を開いた後に PID が再利用され、新しいクライアントが同じ名前へ rename していたら、
// 新しい方の登録は消さない
func TestRemoveStaleKeepsReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "99999.json")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tmp := filepath.Join(dir, ".99999.tmp")
	if err := os.WriteFile(tmp, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	removeStale(f, path)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("replaced registry file was removed: %v", err)
	}
}

func TestFormatForwardRegistryEmpty(t *testing.T) {
	if got := formatForwardRegistry(nil); got != "No port forwards on this machine.\n" {
		t.Errorf("formatForwardRegistry(nil) = %q", got)
	}
}
