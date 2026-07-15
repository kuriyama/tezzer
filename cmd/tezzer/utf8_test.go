package main

import (
	"bytes"
	"testing"
)

func TestUtf8IncompleteTrail(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want int
	}{
		{"empty", []byte{}, 0},
		{"ascii only", []byte("hello"), 0},
		{"complete 2-byte", []byte{0xC3, 0xA9}, 0},             // é
		{"incomplete 2-byte", []byte{0xC3}, 1},                 // é の先頭だけ
		{"complete 3-byte", []byte{0xE3, 0x83, 0x96}, 0},       // ブ
		{"incomplete 3-byte (1)", []byte{0xE3}, 1},             // ブ の先頭だけ
		{"incomplete 3-byte (2)", []byte{0xE3, 0x83}, 2},       // ブ の先頭2バイト
		{"complete 4-byte", []byte{0xF0, 0x9F, 0x98, 0x80}, 0}, // 😀
		{"incomplete 4-byte (1)", []byte{0xF0}, 1},
		{"incomplete 4-byte (2)", []byte{0xF0, 0x9F}, 2},
		{"incomplete 4-byte (3)", []byte{0xF0, 0x9F, 0x98}, 3},
		// ASCII の後に不完全なマルチバイト
		{"ascii then incomplete 3-byte", []byte{'a', 'b', 0xE3}, 1},
		{"ascii then incomplete 3-byte (2)", []byte{'a', 0xE3, 0x83}, 2},
		// 完全なマルチバイトの後にさらに不完全
		{"complete then incomplete", []byte{0xE3, 0x83, 0x96, 0xE3}, 1},
		// 実際のペースト例: 512 バイト境界で分断
		{"paste split at E3", append([]byte("abcdefghij"), 0xE3), 1},
		{"paste split at E3 83", append([]byte("abcdefghij"), 0xE3, 0x83), 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := utf8IncompleteTrail(tt.data)
			if got != tt.want {
				t.Errorf("utf8IncompleteTrail(%v) = %d, want %d", tt.data, got, tt.want)
			}
		})
	}
}

// simulateBatchFlush は flushInputBatch の UTF-8 持ち越しロジックを再現する。
// held（前バッチからの持ち越し）に chunk を足し、不完全な末尾を切り出して
// 「送信するバイト列」と「次に持ち越すバイト列」を返す。
func simulateBatchFlush(held, chunk []byte) (sent, carry []byte) {
	buf := append(append([]byte{}, held...), chunk...)
	trail := utf8IncompleteTrail(buf)
	return buf[:len(buf)-trail], buf[len(buf)-trail:]
}

// TestUtf8CarryoverReassembly はの核心を検証する: マルチバイト文字が
// パケット（バッチ）境界で分断されても、持ち越し再結合で
//
//	(1) 送信される各片が決して UTF-8 シーケンスの途中で終わらない
//	(2) 送信片を連結すると元の入力に一致する
//
// ことを、あらゆる分割点について確認する。
func TestUtf8CarryoverReassembly(t *testing.T) {
	inputs := [][]byte{
		[]byte("ブ"),                          // の文字 (0xE3 0x83 0x96)
		[]byte("abcブdef"),                    // ASCII 混在
		[]byte("héllo"),                      // 2バイト
		[]byte("😀絵文字"),                       // 4バイト + 3バイト
		[]byte("日本語テキスト1234"),                // 連続マルチバイト + ASCII
		[]byte{0x61, 0xE3, 0x83, 0x96, 0x62}, // a ブ b（の分断例の素材）
	}

	for _, input := range inputs {
		// 全ての分割点（0..len）で 2 チャンクに分けて流す
		for split := 0; split <= len(input); split++ {
			chunk1 := input[:split]
			chunk2 := input[split:]

			sent1, carry := simulateBatchFlush(nil, chunk1)
			sent2, carryAfter := simulateBatchFlush(carry, chunk2)

			// (1) 各送信片は不完全シーケンスで終わらない
			if trail := utf8IncompleteTrail(sent1); trail != 0 {
				t.Fatalf("input=%q split=%d: sent1 ends mid-sequence (trail=%d): %v", input, split, trail, sent1)
			}
			if trail := utf8IncompleteTrail(sent2); trail != 0 {
				t.Fatalf("input=%q split=%d: sent2 ends mid-sequence (trail=%d): %v", input, split, trail, sent2)
			}
			// 入力全体は完全な UTF-8 なので最終的な持ち越しは無いはず
			if len(carryAfter) != 0 {
				t.Fatalf("input=%q split=%d: unexpected leftover carry: %v", input, split, carryAfter)
			}
			// (2) 送信片の連結が元入力に一致
			got := append(append([]byte{}, sent1...), sent2...)
			if !bytes.Equal(got, input) {
				t.Fatalf("input=%q split=%d: reassembled %q != input", input, split, got)
			}
		}
	}
}

// TestUtf8CarryoverExact239 はそのもの: 1パケット目が 0xE3 で終わり、
// 2パケット目が 0x83 0x96 で始まるケースで、1パケット目では「ブ」を送らず持ち越し、
// 2パケット目で完全な「ブ」が送られることを確認する。
func TestUtf8CarryoverExact239(t *testing.T) {
	// パケット1: "あ" の後に "ブ" の先頭バイトだけ
	pkt1 := append([]byte("あ"), 0xE3)
	sent1, carry := simulateBatchFlush(nil, pkt1)
	if !bytes.Equal(sent1, []byte("あ")) {
		t.Fatalf("pkt1 should send only 'あ', got %v", sent1)
	}
	if !bytes.Equal(carry, []byte{0xE3}) {
		t.Fatalf("pkt1 should carry 0xE3, got %v", carry)
	}
	// パケット2: "ブ" の残り 0x83 0x96
	sent2, carryAfter := simulateBatchFlush(carry, []byte{0x83, 0x96})
	if !bytes.Equal(sent2, []byte("ブ")) {
		t.Fatalf("pkt2 should send complete 'ブ', got %v", sent2)
	}
	if len(carryAfter) != 0 {
		t.Fatalf("no carry expected after complete char, got %v", carryAfter)
	}
}
