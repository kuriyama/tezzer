package main

// sessions_json_test.go
//
// -list -json / -info -json の出力（formatSessionsListJSON / formatSessionInfoJSON）の
// ゴールデンテスト。フィールド名（snake_case）の互換性を守るのが主目的。
// 時刻は UTC 固定で再現可能にする。
// golden ファイルは `go test ./cmd/tezzer -run Golden -update` で更新する。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/proto"
)

func TestFormatSessionsListJSON_Empty(t *testing.T) {
	got, err := formatSessionsListJSON(nil, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// 空でも "sessions": [] を出す（null にしない）
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	sessions, ok := decoded["sessions"].([]any)
	if !ok || len(sessions) != 0 {
		t.Fatalf("empty: got %q", got)
	}
}

func testSessionsJSONFixture() []proto.SessionInfo {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()
	detached := time.Date(2026, 1, 2, 3, 10, 0, 0, time.UTC).Unix()
	lastSeen := time.Date(2026, 1, 2, 3, 20, 30, 0, time.UTC).Unix()
	lastInput := time.Date(2026, 1, 2, 3, 20, 40, 0, time.UTC).Unix()
	lastOutput := time.Date(2026, 1, 2, 3, 20, 50, 0, time.UTC).Unix()

	return []proto.SessionInfo{
		{
			SessionID:   "sess-AAAAAAAAAAAAAAAA",
			Name:        "work",
			Cmd:         "zsh",
			Rows:        24,
			Cols:        80,
			CreatedAt:   created,
			ClientCount: 2,
			UDPEnabled:  true,
			UDPPort:     7020,
			Clients: []proto.ClientInfo{
				{ID: "c1", Protocol: "UDS"},
				{
					ID: "c2", Protocol: "UDP",
					RemoteAddr:  "198.51.100.2:40000",
					UDPClientID: 1234, UDPAddresses: []string{"198.51.100.2:40000"},
					SendBufferBytes:  4096,
					SlowOutputWrites: 3, MaxOutputWriteMs: 120,
					LastSeen:       lastSeen,
					ForwardsActive: 2, ForwardsOpened: 17,
					ForwardBytesToTarget: 1024, ForwardBytesFromTarget: 65536,
				},
			},
			OutputChunks:      10,
			OutputBufferBytes: 8192,
			OldestChunkTime:   created,
			LastOutputAt:      lastOutput,
			LastInputAt:       lastInput,
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
	}
}

func TestFormatSessionsListJSON_Golden(t *testing.T) {
	got, err := formatSessionsListJSON(testSessionsJSONFixture(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	compareGolden(t, got, filepath.Join("testdata", "sessions_list_json.golden"))
}

func TestFormatSessionInfoJSON_Golden(t *testing.T) {
	got, err := formatSessionInfoJSON(testSessionsJSONFixture()[0], time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	compareGolden(t, got, filepath.Join("testdata", "session_info_json.golden"))
}

func compareGolden(t *testing.T, got, golden string) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
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
