package main

// list_format_test.go
//
// -list の表示組み立て（formatSessionsList）のゴールデンテスト。
// 時刻は UTC 固定で再現可能にする。
// golden ファイルは `go test ./cmd/tezzer -run TestFormatSessionsList_Golden -update` で更新する。

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/proto"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func TestFormatSessionsList_Empty(t *testing.T) {
	got := formatSessionsList(nil, time.UTC)
	if got != "No active sessions\n" {
		t.Fatalf("empty: got %q", got)
	}
}

func TestFormatSessionsList_Golden(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()
	detached := time.Date(2026, 1, 2, 3, 10, 0, 0, time.UTC).Unix()

	sessions := []proto.SessionInfo{
		{
			SessionID:   "sess-AAAAAAAAAAAAAAAA",
			Name:        "work",
			Cmd:         "zsh",
			Rows:        24,
			Cols:        80,
			CreatedAt:   created,
			ClientCount: 2,
			LastUDPAddr: "203.0.113.1:7021",
			Clients: []proto.ClientInfo{
				{ID: "c1", Protocol: "UDS"},
				{ID: "c2", Protocol: "UDP", RemoteAddr: "198.51.100.2:40000", UDPClientID: 1234, UDPAddresses: []string{"198.51.100.2:40000"}},
			},
		},
		{
			SessionID:      "sess-BBBBBBBBBBBBBBBB",
			Cmd:            "vim",
			Rows:           50,
			Cols:           120,
			CreatedAt:      created,
			ClientCount:    0,
			PTYClosed:      true,
			LastDetachedAt: detached,
		},
		{
			// ID が既定幅(22)より長いケース: 列幅が伸びる
			SessionID:   "sess-CCCCCCCCCCCCCCCCCCCCCCCCCC",
			Cmd:         "long-running-command-name",
			Rows:        10,
			Cols:        40,
			CreatedAt:   created,
			ClientCount: 1,
		},
	}

	got := formatSessionsList(sessions, time.UTC)
	golden := filepath.Join("testdata", "sessions_list.golden")

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated golden: %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Fatalf("golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

func TestFormatServerVersion(t *testing.T) {
	got := formatServerVersion(&proto.ServerMetaMsg{ServerVersion: "0.1.2", ServerBuildID: "88800e4f", ServerBuildTime: "2026-09-30_01:02:03_UTC"})
	if want := "tezzerd 0.1.2 (commit 88800e4f, built 2026-09-30_01:02:03_UTC)"; got != want {
		t.Errorf("formatServerVersion = %q, want %q", got, want)
	}
	// ServerVersion を持たない旧サーバ（attach 後の GET_SERVER_META には応答する）
	got = formatServerVersion(&proto.ServerMetaMsg{ServerBuildID: "57029707", ServerBuildTime: "x"})
	if want := "tezzerd unknown (commit 57029707, built x)"; got != want {
		t.Errorf("formatServerVersion(old) = %q, want %q", got, want)
	}
}

func TestFormatRemoteForwards(t *testing.T) {
	if got := formatRemoteForwards(nil, time.UTC); got != "" {
		t.Fatalf("empty = %q", got)
	}
	got := formatRemoteForwards([]proto.RemoteForwardInfo{
		{Port: 9222, Device: "aaaaaaaa", Live: 2, Dormant: 1, Active: 1, Opened: 5, ListenFrom: 1790000000},
		{Port: 9223, Waiting: 1, BindError: "address already in use"},
	}, time.UTC)
	for _, want := range []string{
		"REMOTE FORWARDS (-R)",
		"9222   aaaaaaaa  2 live/1 dormant/0 wait",
		"listening since 2026-09-21 14:13:20",
		"9223   -         0 live/0 dormant/1 wait",
		"bind failed: address already in use",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatSessionsListJSONRemoteForwards(t *testing.T) {
	got, err := formatSessionsListJSON(nil, []proto.RemoteForwardInfo{{Port: 9222, Device: "aaaaaaaa", Live: 1, ListenFrom: 1790000000}}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"port": 9222`, `"listening": true`, `"live_clients": 1`, `"listening_since": "2026-09-21T14:13:20Z"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
}
