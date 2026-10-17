package session

// rforward_test.go: -R の登録表（rfwdRegistry）のテスト。transport は偽物を使い、
// listener は実際に loopback で listen して確認する。

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

// fakeRFwdTransport は SendStatus と OpenRemoteForward だけを実装する偽 transport。
// OpenRemoteForward は target（クライアント側の dial 先の代わり）へ直接 TCP 接続する。
type fakeRFwdTransport struct {
	transport.ServerTransport // 未使用メソッドは呼ばれない（呼ばれたら nil で panic）

	target string

	mu       sync.Mutex
	statuses map[transport.ClientID][]string
	opens    map[transport.ClientID]int
}

func newFakeRFwdTransport(target string) *fakeRFwdTransport {
	return &fakeRFwdTransport{target: target, statuses: map[transport.ClientID][]string{}, opens: map[transport.ClientID]int{}}
}

func (f *fakeRFwdTransport) SendStatus(id transport.ClientID, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[id] = append(f.statuses[id], msg)
	return nil
}

func (f *fakeRFwdTransport) lastStatus(id transport.ClientID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.statuses[id]
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

func (f *fakeRFwdTransport) openCount(id transport.ClientID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[id]
}

func (f *fakeRFwdTransport) OnRemoteForward(func(transport.ClientID, string, []uint16), func(transport.ClientID, bool)) {
}

func (f *fakeRFwdTransport) OpenRemoteForward(ctx context.Context, id transport.ClientID, port uint16) (transport.ForwardConn, error) {
	f.mu.Lock()
	f.opens[id]++
	f.mu.Unlock()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", f.target)
	if err != nil {
		return nil, err
	}
	return c.(*net.TCPConn), nil
}

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// freePort は空いている TCP ポートを返す（閉じてから返すので、まれに競合しうる）。
func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return uint16(port)
}

func portAddr(port uint16) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
}

// roundTrip は port に接続して msg を送り、エコーが返るかを返す。
func roundTrip(t *testing.T, port uint16) bool {
	t.Helper()
	c, err := net.DialTimeout("tcp", portAddr(port), 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return false
	}
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	return string(got) == "ping"
}

func listening(port uint16) bool {
	c, err := net.DialTimeout("tcp", portAddr(port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func newTestRegistry(t *testing.T) *rfwdRegistry {
	r := newRFwdRegistry()
	t.Cleanup(r.closeAll)
	return r
}

const (
	devA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	devB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestRFwd_RelayAndBundleSameDevice(t *testing.T) {
	r := newTestRegistry(t)
	st := newFakeRFwdTransport(startEcho(t))
	port := freePort(t)
	c1 := transport.ClientID{Session: "s1", Num: 1}
	c2 := transport.ClientID{Session: "s2", Num: 1}

	r.hello(st, c1, devA, []uint16{port})
	r.hello(st, c2, devA, []uint16{port})
	if !roundTrip(t, port) {
		t.Fatal("relay failed")
	}
	// 最後に Hello した c2 が選ばれる。
	if st.openCount(c2) != 1 || st.openCount(c1) != 0 {
		t.Fatalf("opens c1=%d c2=%d, want c2 only", st.openCount(c1), st.openCount(c2))
	}
	if !strings.Contains(st.lastStatus(c1), "listening") || !strings.Contains(st.lastStatus(c2), "listening") {
		t.Fatalf("statuses c1=%q c2=%q", st.lastStatus(c1), st.lastStatus(c2))
	}

	// c2 が明示的に終了しても c1 が引き継ぐ。
	r.gone(c2, true)
	if !roundTrip(t, port) {
		t.Fatal("relay failed after one client left")
	}
	if st.openCount(c1) != 1 {
		t.Fatalf("c1 opens = %d, want 1", st.openCount(c1))
	}

	// 全員が明示的に終了したら listener を手放す。
	r.gone(c1, true)
	if listening(port) {
		t.Fatal("still listening after all clients exited")
	}
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("snapshot = %+v, want empty", got)
	}
}

func TestRFwd_OtherDeviceWaitsThenTakesOver(t *testing.T) {
	r := newTestRegistry(t)
	stA := newFakeRFwdTransport(startEcho(t))
	stB := newFakeRFwdTransport(startEcho(t))
	port := freePort(t)
	a := transport.ClientID{Session: "s1", Num: 1}
	b := transport.ClientID{Session: "s2", Num: 1}

	r.hello(stA, a, devA, []uint16{port})
	r.hello(stB, b, devB, []uint16{port})
	if !strings.Contains(stB.lastStatus(b), "another device") {
		t.Fatalf("B status = %q, want busy notice", stB.lastStatus(b))
	}
	if !roundTrip(t, port) || stA.openCount(a) != 1 || stB.openCount(b) != 0 {
		t.Fatal("relay should go to device A while it owns the port")
	}
	if snap := r.snapshot(); len(snap) != 1 || snap[0].Live != 1 || snap[0].Waiting != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// A が明示的に終了したら、待機していた B に引き継がれる。
	r.gone(a, true)
	if !strings.Contains(stB.lastStatus(b), "listening") {
		t.Fatalf("B status = %q, want listening after takeover", stB.lastStatus(b))
	}
	if !roundTrip(t, port) || stB.openCount(b) != 1 {
		t.Fatal("relay should go to device B after takeover")
	}
}

func TestRFwd_DormantHoldsPortAndRefuses(t *testing.T) {
	r := newTestRegistry(t)
	now := time.Now()
	r.now = func() time.Time { return now }
	st := newFakeRFwdTransport(startEcho(t))
	stB := newFakeRFwdTransport(startEcho(t))
	port := freePort(t)
	a := transport.ClientID{Session: "s1", Num: 1}
	b := transport.ClientID{Session: "s2", Num: 1}

	r.hello(st, a, devA, []uint16{port})
	r.gone(a, false) // スリープ等で接続喪失 → 休眠

	if !listening(port) {
		t.Fatal("dormant port should stay bound")
	}
	if roundTrip(t, port) || st.openCount(a) != 0 {
		t.Fatal("dormant port should refuse connections")
	}
	// 休眠中は別端末に渡さない（どの端末の Chrome か分からなくなるのを防ぐ）。
	r.hello(stB, b, devB, []uint16{port})
	if !strings.Contains(stB.lastStatus(b), "another device") {
		t.Fatalf("B status = %q, want busy while A is dormant", stB.lastStatus(b))
	}

	// 同じクライアントが reconnect すれば復帰する。
	r.hello(st, a, devA, []uint16{port})
	if !roundTrip(t, port) {
		t.Fatal("relay failed after reconnect")
	}

	// 再び休眠させ、猶予期間を過ぎたら失効して B に引き継がれる。
	r.gone(a, false)
	now = now.Add(rfwdDormantTTL + time.Minute)
	r.tick()
	if !strings.Contains(stB.lastStatus(b), "listening") {
		t.Fatalf("B status = %q, want listening after A expired", stB.lastStatus(b))
	}
	if !roundTrip(t, port) || stB.openCount(b) != 1 {
		t.Fatal("relay should go to B after A's dormant registration expired")
	}
}

func TestRFwd_BindFailureRetries(t *testing.T) {
	r := newTestRegistry(t)
	st := newFakeRFwdTransport(startEcho(t))
	// ホスト上の別プロセスがポートを使っている状態。
	blocker, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(blocker.Addr().(*net.TCPAddr).Port)
	a := transport.ClientID{Session: "s1", Num: 1}

	r.hello(st, a, devA, []uint16{port})
	if !strings.Contains(st.lastStatus(a), "cannot listen") {
		t.Fatalf("status = %q, want bind failure notice", st.lastStatus(a))
	}
	if snap := r.snapshot(); len(snap) != 1 || snap[0].BindError == "" || snap[0].Waiting != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}

	blocker.Close()
	r.tick()
	if !strings.Contains(st.lastStatus(a), "listening") {
		t.Fatalf("status = %q, want listening after retry", st.lastStatus(a))
	}
	if !roundTrip(t, port) {
		t.Fatal("relay failed after retry")
	}
}

func TestRFwd_DropSessionReleases(t *testing.T) {
	r := newTestRegistry(t)
	st := newFakeRFwdTransport(startEcho(t))
	port := freePort(t)
	a := transport.ClientID{Session: "s1", Num: 1}

	r.hello(st, a, devA, []uint16{port})
	r.gone(a, false) // 休眠中でも
	r.dropSession("s1")
	if listening(port) {
		t.Fatal("port still bound after the session was deleted")
	}
}

func TestRFwd_NoDuplicateNotices(t *testing.T) {
	r := newTestRegistry(t)
	st := newFakeRFwdTransport(startEcho(t))
	port := freePort(t)
	a := transport.ClientID{Session: "s1", Num: 1}
	r.hello(st, a, devA, []uint16{port})
	r.tick()
	r.tick()
	st.mu.Lock()
	n := len(st.statuses[a])
	st.mu.Unlock()
	if n != 1 {
		t.Fatalf("got %d notices, want 1", n)
	}
}
