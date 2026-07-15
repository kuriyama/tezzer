package qtransport

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"testing"
	"time"
)

// inputDeduper の単体テスト。ストリーム/DATAGRAM のどちらが先に届いても
// 一度だけ・順序どおりに転送されることを確認する。

func TestInputDeduper_StreamOnly(t *testing.T) {
	var d inputDeduper
	if got := d.fromStream([]byte("abc")); string(got) != "abc" {
		t.Fatalf("fromStream = %q, want %q", got, "abc")
	}
	if got := d.fromStream([]byte("def")); string(got) != "def" {
		t.Fatalf("fromStream = %q, want %q", got, "def")
	}
}

func TestInputDeduper_DatagramFirstThenStream(t *testing.T) {
	var d inputDeduper
	// datagram が先に届く
	if got := d.fromDatagram(0, []byte("abc")); string(got) != "abc" {
		t.Fatalf("fromDatagram = %q, want %q", got, "abc")
	}
	// 同じ内容がストリームから届く → 全部重複
	if got := d.fromStream([]byte("abc")); got != nil {
		t.Fatalf("fromStream = %q, want nil", got)
	}
	// 続きはそのまま通る
	if got := d.fromStream([]byte("def")); string(got) != "def" {
		t.Fatalf("fromStream = %q, want %q", got, "def")
	}
}

func TestInputDeduper_StreamFirstThenDatagram(t *testing.T) {
	var d inputDeduper
	if got := d.fromStream([]byte("abc")); string(got) != "abc" {
		t.Fatalf("fromStream = %q, want %q", got, "abc")
	}
	// 遅れて届いた datagram は重複として落ちる
	if got := d.fromDatagram(0, []byte("abc")); got != nil {
		t.Fatalf("fromDatagram = %q, want nil", got)
	}
}

func TestInputDeduper_PartialOverlap(t *testing.T) {
	var d inputDeduper
	// datagram が [0,6) を適用
	if got := d.fromDatagram(0, []byte("abcdef")); string(got) != "abcdef" {
		t.Fatalf("fromDatagram = %q, want %q", got, "abcdef")
	}
	// ストリームが [0,3) → 全部適用済み
	if got := d.fromStream([]byte("abc")); got != nil {
		t.Fatalf("fromStream = %q, want nil", got)
	}
	// ストリームが [3,9) → 前半 [3,6) は適用済み、後半 [6,9) だけ通る
	if got := d.fromStream([]byte("defghi")); string(got) != "ghi" {
		t.Fatalf("fromStream = %q, want %q", got, "ghi")
	}
}

func TestInputDeduper_DatagramAhead(t *testing.T) {
	var d inputDeduper
	// 手前のストリームデータ未着の datagram はバッファせず破棄
	if got := d.fromDatagram(5, []byte("xyz")); got != nil {
		t.Fatalf("fromDatagram = %q, want nil", got)
	}
	// ストリームが全部届けば欠けなし
	if got := d.fromStream([]byte("01234xyz")); string(got) != "01234xyz" {
		t.Fatalf("fromStream = %q, want %q", got, "01234xyz")
	}
	// 破棄した datagram の再適用はされない
	if got := d.fromDatagram(5, []byte("xyz")); got != nil {
		t.Fatalf("fromDatagram = %q, want nil", got)
	}
}

func TestInputDeduper_ConsecutiveDatagrams(t *testing.T) {
	var d inputDeduper
	if got := d.fromDatagram(0, []byte("ab")); string(got) != "ab" {
		t.Fatalf("fromDatagram = %q, want %q", got, "ab")
	}
	// 連続する datagram はストリームを待たずに適用できる
	if got := d.fromDatagram(2, []byte("cd")); string(got) != "cd" {
		t.Fatalf("fromDatagram = %q, want %q", got, "cd")
	}
	if got := d.fromStream([]byte("abcd")); got != nil {
		t.Fatalf("fromStream = %q, want nil", got)
	}
}

func TestInputDatagramEncodeDecode(t *testing.T) {
	b := encodeInputDatagram(42, []byte("hello"))
	offset, data, ok := decodeInputDatagram(b)
	if !ok || offset != 42 || string(data) != "hello" {
		t.Fatalf("decode = (%d, %q, %v), want (42, %q, true)", offset, data, ok, "hello")
	}
	if _, _, ok := decodeInputDatagram([]byte{1, 2, 3}); ok {
		t.Fatal("decode of short datagram should fail")
	}
}

// TestDatagramInput_EndToEnd は入力の DATAGRAM 二重送信込みで、サーバに届く入力が
// 送信順どおり・重複なしであることを検証する（小さい入力＝二重送信対象、
// 閾値超え＝ストリームのみ、の両方を混ぜる）。
func TestDatagramInput_EndToEnd(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	cli, err := NewClient(k, addr, 7, "sess-dgram")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}

	// 小さい入力（二重送信対象）を多数 + 閾値超えの大きい入力（ストリームのみ）を混ぜる
	var want bytes.Buffer
	send := func(p []byte) {
		if err := cli.SendInput(p); err != nil {
			t.Fatalf("SendInput: %v", err)
		}
		want.Write(p)
	}
	for i := 0; i < 100; i++ {
		send([]byte{'a' + byte(i%26)})
	}
	big := bytes.Repeat([]byte("P"), maxDupInputSize+100) // ペースト相当
	send(big)
	for i := 0; i < 100; i++ {
		send([]byte{'A' + byte(i%26)})
	}

	// 期待バイト数がそろうまで受信し、内容が送信順どおり・重複なしであることを確認
	var got bytes.Buffer
	deadline := time.After(10 * time.Second)
	for got.Len() < want.Len() {
		select {
		case in := <-srv.Input():
			got.Write(in.Data)
		case <-deadline:
			t.Fatalf("timeout: got %d bytes, want %d", got.Len(), want.Len())
		}
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("input mismatch: got %d bytes, want %d bytes", got.Len(), want.Len())
	}

	// 重複が余分に届いていないこと（datagram の重複は dedup が落とすはず）
	select {
	case in := <-srv.Input():
		t.Fatalf("unexpected extra input: %q", in.Data)
	case <-time.After(300 * time.Millisecond):
	}
}
