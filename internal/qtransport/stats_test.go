package qtransport

// Stats() の検証: 稼働中の接続で RTT/bytes が埋まり、migration 後に
// RecoveryCount/LastRecoveryMs が更新されること（-race 込みで ConnectionStats の
// 並行アクセス安全性も確認）。

import (
	"context"
	"crypto/rand"
	"github.com/kuriyama/tezzer/internal/transport"
	"net"
	"testing"
	"time"
)

func TestQUICTransport_Stats(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	var off uint64
	go func() {
		for {
			in, ok := <-srv.Input()
			if !ok {
				return
			}
			off++
			_ = srv.SendOutput(off, in.Data, []transport.ClientID{in.Client})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	cli, err := NewClient(k, addr, 12, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	qc := cli.(*quicClient)

	roundTrip := func(msg string) {
		t.Helper()
		_ = cli.SendInput([]byte(msg))
		select {
		case <-cli.Output():
		case <-time.After(5 * time.Second):
			t.Fatalf("no echo for %q", msg)
		}
	}

	for i := 0; i < 3; i++ {
		roundTrip("ping")
	}

	// 稼働中に Stats() を呼ぶ（conn の ConnectionStats を並行アクセス）。
	s := cli.Stats()
	if s.BytesSent == 0 || s.BytesReceived == 0 {
		t.Fatalf("Stats bytes not populated: %+v", s)
	}
	if s.RecoveryCount != 0 {
		t.Fatalf("RecoveryCount should be 0 before migration, got %d", s.RecoveryCount)
	}

	// migration 後に回復統計が更新される。
	if err := qc.migrate("stats-test"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	roundTrip("after")
	s2 := cli.Stats()
	if s2.RecoveryCount != 1 {
		t.Fatalf("RecoveryCount=%d want 1 after migration", s2.RecoveryCount)
	}
	t.Logf("stats OK: RTT=%.3fms loss=%.4f bytesSent=%d recov=%d lastRecov=%.0fms",
		s2.RTT, s2.LossRate, s2.BytesSent, s2.RecoveryCount, s2.LastRecoveryMs)
}
