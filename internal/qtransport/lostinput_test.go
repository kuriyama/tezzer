package qtransport

// 届かなかった入力の通知（lostinput.go）の検証。

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLostInput_ResolveOrdersPreviousStreamFirst(t *testing.T) {
	var l lostInput
	l.recordSent([]byte("ls\n"))
	l.recordSent([]byte("make deploy\n"))
	st := l.newStream(15)          // 前のストリームに 15 バイト書いた
	l.addLost([]byte("echo hi\n")) // 再接続を待つ間に送れず捨てた
	if total, _ := l.take(); total != 0 {
		t.Fatalf("take() before the server's position = %d, want 0 (wait for it)", total)
	}
	applied := uint64(3) // サーバに届いたのは "ls\n" だけ
	l.resolve(&applied, st)
	total, head := l.take()
	if total != 20 || string(head) != "make deploy\necho hi\n" {
		t.Errorf("take() = (%d, %q), want (20, %q)", total, head, "make deploy\necho hi\n")
	}
	if total, _ := l.take(); total != 0 {
		t.Errorf("second take() = %d, want 0", total)
	}
}

func TestLostInput_UnknownPositionReportsDroppedOnly(t *testing.T) {
	var l lostInput
	l.recordSent([]byte("abc"))
	st := l.newStream(3)
	l.addLost([]byte("xyz"))
	l.resolve(nil, st) // 旧サーバ・サーバ再起動後は前の接続の分が分からない
	if total, head := l.take(); total != 3 || string(head) != "xyz" {
		t.Errorf("take() = (%d, %q), want (3, %q)", total, head, "xyz")
	}
}

// 差し替え直後に古い control ストリームで読めた位置は、新しい差し替えの位置として使わない。
func TestLostInput_IgnoresPositionFromStaleStream(t *testing.T) {
	var l lostInput
	old := l.newStream(0) // 1 回目の再接続（この接続の control で前の接続の位置を読む）
	l.recordSent([]byte("make deploy\n"))
	cur := l.newStream(12) // すぐ 2 回目の再接続
	applied := uint64(0)   // 1 回目の接続が読んだ位置（さらに前の接続の分）が遅れて届く
	l.resolve(&applied, old)
	if total, _ := l.take(); total != 0 {
		t.Fatalf("take() after a stale position = %d, want 0 (still waiting)", total)
	}
	applied = 12 // 今の接続が読んだ位置: 全部届いていた
	l.resolve(&applied, cur)
	if total, head := l.take(); total != 0 {
		t.Errorf("take() = (%d, %q), want nothing lost", total, head)
	}
}

func TestLostInput_HeadIsBounded(t *testing.T) {
	var l lostInput
	l.addLost([]byte(strings.Repeat("a", lostHeadMax+100)))
	total, head := l.take()
	if total != lostHeadMax+100 || len(head) != lostHeadMax {
		t.Errorf("take() = (%d, %d bytes of head), want (%d, %d)", total, len(head), lostHeadMax+100, lostHeadMax)
	}
}

// 書き込みは成功したが届く前に経路が切れ、別経路で再接続した場合に、届かなかった入力を
// サーバからの位置で検出して知らせること。
func TestQUICTransport_ReportsInputLostAcrossReconnect(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	inputs := inputChan(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	relay := newBlackholeRelay(t, srv.Addr().String())

	var mu sync.Mutex
	var lostTotal int
	var lostHead []byte
	lostCh := make(chan struct{}, 1)
	cli := NewClient(ClientConfig{
		Key: k, Candidates: []string{relay.Addr()}, ClientID: 9,
		OnInputLost: func(total int, head []byte) {
			mu.Lock()
			lostTotal, lostHead = total, head
			mu.Unlock()
			lostCh <- struct{}{}
		},
	})
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	if err := cli.SendInput([]byte("ls\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	select {
	case <-inputs:
	case <-time.After(3 * time.Second):
		t.Fatal("first input did not arrive")
	}

	relay.drop.Store(true)
	if err := cli.SendInput([]byte("make deploy\n")); err != nil {
		t.Fatalf("SendInput while the path is dead: %v", err) // 書き込み自体は成功する
	}
	// 中継を通らない経路で再接続する
	cli.SetRoamingCandidates([]string{srv.Addr().String()})
	if err := cli.reconnect("test"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	select {
	case <-lostCh:
	case <-time.After(5 * time.Second):
		t.Fatal("OnInputLost was not called after the reconnect")
	}
	mu.Lock()
	defer mu.Unlock()
	if lostTotal != len("make deploy\n") || string(lostHead) != "make deploy\n" {
		t.Errorf("OnInputLost(%d, %q), want (%d, %q)", lostTotal, lostHead, len("make deploy\n"), "make deploy\n")
	}
}

// 入力が全部届いていれば、再接続しても OnInputLost は呼ばれない（誤報しない）。
func TestQUICTransport_NoInputLostReportWhenDelivered(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	inputs := inputChan(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	lostCh := make(chan int, 1)
	cli := NewClient(ClientConfig{
		Key: k, Candidates: []string{srv.Addr().String()}, ClientID: 10,
		OnInputLost: func(total int, _ []byte) { lostCh <- total },
	})
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	if err := cli.SendInput([]byte("ls\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	select {
	case <-inputs:
	case <-time.After(3 * time.Second):
		t.Fatal("input did not arrive")
	}
	if err := cli.reconnect("test"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if err := cli.SendInput([]byte("pwd\n")); err != nil {
		t.Fatalf("SendInput after reconnect: %v", err)
	}
	select {
	case total := <-lostCh:
		t.Fatalf("OnInputLost(%d) although every input was delivered", total)
	case <-time.After(time.Second):
	}
}

// 入力の書き込み（OnInput）が詰まったまま（入力を読まないプロセスへの貼り付け）でも、
// 意図した終了の後始末（OnClientExit）が止まらないこと。
func TestQUICTransport_ClientExitWhileInputBlocked(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()
	unblock := make(chan struct{})
	defer close(unblock)
	received := make(chan struct{}, 1)
	srv.OnInput(func(Input) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-unblock // PTY が読まれないまま詰まっている
	})
	exited := make(chan ClientID, 1)
	srv.OnClientExit(func(cid ClientID) { exited <- cid })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	cli := NewClient(ClientConfig{Key: k, Candidates: []string{srv.Addr().String()}, ClientID: 11})
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	if err := cli.SendInput([]byte("paste")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("input did not reach OnInput")
	}
	cli.Close() // 意図した終了
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("OnClientExit was not called while the input write was blocked")
	}
}

// 届かなかった量が手元に残している量を超えたら、途中からを冒頭として見せず数だけにする。
func TestLostInput_HeadUnknownWhenBeyondKept(t *testing.T) {
	var l lostInput
	l.recordSent([]byte(strings.Repeat("a", sentKeep+10)))
	st := l.newStream(uint64(sentKeep + 10))
	l.addLost([]byte("later"))
	applied := uint64(0)
	l.resolve(&applied, st)
	total, head := l.take()
	if total != sentKeep+10+len("later") || head != nil {
		t.Errorf("take() = (%d, %d bytes of head), want (%d, no head)", total, len(head), sentKeep+10+len("later"))
	}
}
