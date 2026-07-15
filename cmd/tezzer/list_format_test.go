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
