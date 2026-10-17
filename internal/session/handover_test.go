package session

// 無停止再起動（SIGUSR2 handover、handover.go）の WriteHandover/RestoreHandover
// 状態往復テスト。実装済みなのに（execve を伴う実プロセス置換を除けば）テストが
// 一本もなかったため追加した。
//
// exec は行わない（同一プロセス内で旧 Manager → バッファ → 新 Manager と往復させる）。
// WriteHandover は成功時に全セッションの mu を握ったまま返す契約（呼び出し元が
// exec する前提）なので、旧セッションの ptyMaster/transport をそのまま放置すると
// 「複製前の fd」と「複製後の fd（新 Manager が使う）」が同じ PTY/UDP ソケットを
// 指したまま両方生きた状態になり、以後に書き込む新しいデータをどちらが読むか
// 競合する。本物の再起動では旧プロセスが exec で消えるため起きない競合なので、
// テストでは旧セッションの ptyMaster と transport を直接 Close して「旧プロセスは
// もういない」状態を模擬する（Session.Close() は WriteHandover が握ったままの
// mu を取ろうとしてデッドロックするため使えない）。

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/qtransport"
	"github.com/vmihailenco/msgpack/v5"
)

// waitForEcho は client.OutCh から OUTPUT メッセージをデコードしながら読み進め、
// 累積したデータに want が現れるまで待つ（1回の PTY read で必ず1メッセージに
// 収まる保証はなく、途中で分割されうるため単発の受信だけでは判定できない）。
func waitForEcho(t *testing.T, ch <-chan []byte, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	var acc []byte
	for {
		select {
		case raw := <-ch:
			msg, err := proto.Decode(raw)
			if err != nil {
				t.Fatalf("proto.Decode: %v", err)
			}
			out, ok := msg.(*proto.OutputMsg)
			if !ok {
				continue
			}
			acc = append(acc, out.Data...)
			if strings.Contains(string(acc), want) {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q in echo (got so far: %q)", want, acc)
		}
	}
}

// drainOutput は d の間 client.OutCh から OUTPUT メッセージを読み、データを連結して返す。
func drainOutput(t *testing.T, ch <-chan []byte, d time.Duration) []byte {
	t.Helper()
	var acc []byte
	timeout := time.After(d)
	for {
		select {
		case raw := <-ch:
			msg, err := proto.Decode(raw)
			if err != nil {
				t.Fatalf("proto.Decode: %v", err)
			}
			if out, ok := msg.(*proto.OutputMsg); ok {
				acc = append(acc, out.Data...)
			}
		case <-timeout:
			return acc
		}
	}
}

func TestHandover_RoundTrip(t *testing.T) {
	mgr := NewManager()

	sess, err := mgr.CreateSession("handover-rt", "/bin/cat", nil, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	oldClient := sess.AttachClient(0, "TEST", "old-client", 0)
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	if err := sess.WriteInput([]byte("before-handover\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}
	waitForEcho(t, oldClient.OutCh, "before-handover", 2*time.Second)

	// 端末モードの状態も引き継がれること（ptyReader と同じ経路に直接流す）
	sess.handlePTYOutput([]byte("\x1b[?1049h\x1b[?1000;1006h"))
	wantModes := "\x1b[?1049h\x1b[?1000h\x1b[?1006h"

	oldPort := sess.GetQUICPort()
	oldKey := append([]byte(nil), sess.GetQUICKey()...)

	var buf bytes.Buffer
	if _, err := mgr.WriteHandover(&buf); err != nil {
		t.Fatalf("WriteHandover: %v", err)
	}
	// seq とサイズは WriteHandover が mu を握った後に読む。cat のエコーは tty エコーと
	// cat 出力の 2 回届き、waitForEcho は 1 回目で抜けるため、先に読むと後続チャンクで
	// seq がずれる。サイズも attach 時の resize trick（cols-1 → cols）の途中で保存され
	// うるので、24x80 固定ではなく保存時点の値と比べる。
	oldSeq := sess.seq
	oldRows, oldCols := sess.Rows, sess.Cols
	// 「旧プロセスはもういない」を模擬（上のコメント参照）。
	_ = sess.ptyMaster.Close()
	_ = sess.st.Close()

	mgr2 := NewManager()
	defer mgr2.Close()
	n, err := mgr2.RestoreHandover(&buf)
	if err != nil {
		t.Fatalf("RestoreHandover: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored %d session(s), want 1", n)
	}

	restored, err := mgr2.GetSession(sess.ID)
	if err != nil {
		t.Fatalf("GetSession after restore: %v", err)
	}
	defer restored.Close()

	if restored.Name != "handover-rt" {
		t.Errorf("restored Name = %q, want %q", restored.Name, "handover-rt")
	}
	if restored.Cmd != "/bin/cat" {
		t.Errorf("restored Cmd = %q, want /bin/cat", restored.Cmd)
	}
	if restored.Rows != oldRows || restored.Cols != oldCols {
		t.Errorf("restored size = %dx%d, want %dx%d", restored.Rows, restored.Cols, oldRows, oldCols)
	}
	if restored.seq != oldSeq {
		t.Errorf("restored seq = %d, want %d", restored.seq, oldSeq)
	}
	if got := string(restored.TermModeRestore()); got != wantModes {
		t.Errorf("restored TermModeRestore = %q, want %q", got, wantModes)
	}
	if len(restored.outputChunks) == 0 {
		t.Fatal("restored outputChunks is empty, want the pre-handover echo carried over")
	}
	// 1 回の PTY read が 1 チャンクになるので、エコーが複数チャンクに分かれうる
	var restoredOut []byte
	for _, ch := range restored.outputChunks {
		restoredOut = append(restoredOut, ch.Data...)
	}
	if !strings.Contains(string(restoredOut), "before-handover") {
		t.Errorf("restored outputChunks does not contain the pre-handover echo: %+v", restored.outputChunks)
	}
	if restored.GetQUICPort() != oldPort {
		t.Errorf("restored QUIC port = %d, want %d (fd handover must keep the same socket)", restored.GetQUICPort(), oldPort)
	}
	if string(restored.GetQUICKey()) != string(oldKey) {
		t.Error("restored QUIC key does not match original")
	}

	// PTY が復元後も生きていることを、新しい入力の実エコーで確認する。attach 直後は
	// 引き継いだ hot バッファ（"before-handover"）が resync として先に流れてくるので、
	// "after-handover" が出るまで読み飛ばす。
	// 旧セッションの PTY reader goroutine が read 中だと、エコーを先に読んだうえで
	// WriteHandover が握ったままの mu で止まり、そのデータは失われる（本物の再起動では
	// 旧プロセスが exec で消えるので起きない、テスト固有の競合）。奪われるのは高々
	// 1 回なので、エコーが届くまで入力を送り直す。
	newClient := restored.AttachClient(0, "TEST", "new-client", 0)
	var acc []byte
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(string(acc), "after-handover") {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q in echo (got so far: %q)", "after-handover", acc)
		}
		if err := restored.WriteInput([]byte("after-handover\n")); err != nil {
			t.Fatalf("WriteInput on restored session: %v", err)
		}
		acc = append(acc, drainOutput(t, newClient.OutCh, 300*time.Millisecond)...)
	}

	// QUIC トランスポートも fd 越しに生きていることを、実クライアントの接続で確認する。
	cli, err := qtransport.NewClient(oldKey, "127.0.0.1:"+strconv.Itoa(oldPort), 4242, "")
	if err != nil {
		t.Fatalf("qtransport.NewClient: %v", err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("restored QUIC transport did not accept a new client: %v", err)
	}
}

// TestHandover_VersionMismatchRejected は将来のフォーマット変更時に「セッションは
// 失うがサーバは起動する」（黙って個別セッションだけ捨てる、ではなく明示エラー）
// という契約を固定する回帰テスト。
func TestHandover_VersionMismatchRejected(t *testing.T) {
	mgr := NewManager()
	sess, err := mgr.CreateSession("", "/bin/cat", nil, nil, "", 24, 80, false, 0, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	var buf bytes.Buffer
	if _, err := mgr.WriteHandover(&buf); err != nil {
		t.Fatalf("WriteHandover: %v", err)
	}
	_ = sess.ptyMaster.Close()
	_ = sess.st.Close()

	// バージョンフィールドだけ壊す（デコード→書き換え→再エンコード）。
	var raw map[string]any
	if err := msgpack.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("decode for mutation: %v", err)
	}
	raw["v"] = HandoverVersion + 1
	mutated, err := msgpack.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode mutated state: %v", err)
	}

	mgr2 := NewManager()
	defer mgr2.Close()
	if _, err := mgr2.RestoreHandover(bytes.NewReader(mutated)); err == nil {
		t.Fatal("RestoreHandover with mismatched version succeeded, want error")
	}
}
