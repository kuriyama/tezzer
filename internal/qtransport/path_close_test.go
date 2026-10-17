package qtransport

import (
	"context"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// Probe が失敗した path を接続終了後に閉じても panic しないこと
// （quic-go v0.60 の Path.Close は connIDManager の assertNotClosed で panic する。
// migration の Probe 中に接続が死んだ場合に実機で発生した）。
func TestClosePathAfterConnClosed(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	ln := startEchoServer(t, k, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uconn1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tr1 := &quic.Transport{Conn: uconn1}
	defer tr1.Close()
	conn, err := tr1.Dial(ctx, ln.Addr(), clientTLSForKey(t, k, k), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// 新しい経路は不通にして Probe を失敗させる（PATH_CHALLENGE を送ったまま残る）。
	gc := newGatedConn(t)
	gc.setBlocked(true)
	tr := &quic.Transport{Conn: gc}
	defer tr.Close()
	path, err := conn.AddPath(tr)
	if err != nil {
		t.Fatalf("AddPath: %v", err)
	}
	pctx, pcancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer pcancel()
	if err := path.Probe(pctx); err == nil {
		t.Fatal("Probe succeeded on a blocked path")
	}

	_ = conn.CloseWithError(0, "")
	<-conn.Context().Done()
	closePath(conn, path)
}
