//go:build chaos

package qtransport

// カオステスト: 実 UDP ソケット + 実 quic-go の上で、chaosRelay（chaos_relay_test.go）が
// 注入するランダムな loss/duplicate/delay の下で qtransport 自身のロジック（出力ストリーム、
// 入力の DATAGRAM+ストリーム二重送信の dedup）が破綻しないことを検証する。
//
// synctest 非対応（quic-go は実タイマー・実 goroutine で動く）なので実時間依存。
// make test / make ci には含めず、`make test-chaos` 専用（e2e/e2e-docker と同じ扱い）。
//
// seed は t.Logf で記録し、TEZZER_TEST_SEED で単一 seed を再現できる
// （テスト戦略ドキュメントの sim テストと同じ運用ルール）。

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

func chaosSeeds(t *testing.T, defaults []uint64) []uint64 {
	t.Helper()
	if s := os.Getenv("TEZZER_TEST_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("TEZZER_TEST_SEED: %v", err)
		}
		return []uint64{v}
	}
	return defaults
}

// TestChaos_ModerateLossDelayDup_DataArrivesIntact は中程度の障害率（接続が死なない範囲）で
// 出力ストリームと入力（dedup 対象）がどちらも欠落・重複なく完全に届くことを検証する。
// QUIC 自体のストリーム再送に加え、tezzer 側の入力 dedup（input_dedup.go）が正しく
// 機能していることの保険になる。
func TestChaos_ModerateLossDelayDup_DataArrivesIntact(t *testing.T) {
	for _, seed := range chaosSeeds(t, []uint64{1, 2, 3, 4, 5}) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			runChaosModerateLoss(t, seed)
		})
	}
}

func runChaosModerateLoss(t *testing.T, seed uint64) {
	t.Helper()
	t.Logf("seed=%d", seed)

	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	srv, err := NewServer(k, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer srv.Close()

	// SendOutput はサーバー側に client がまだ登録されていない（s.clients[cid]==nil）と
	// エラーなく黙ってスキップされる。cli.Start() が返っても、サーバー側の登録
	// （Hello 処理）が終わっている保証はないため、OnClientConnect で明示的に待つ。
	connected := make(chan struct{})
	srv.OnClientConnect(func(transport.ClientID) { close(connected) })

	// ctx は server/client の生存期間そのもの（内部で context.WithCancel(ctx) される）
	// なので、下の受信待ちループの上限より確実に長く取る。
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	addr := srv.(interface{ Addr() net.Addr }).Addr().String()

	policy := chaosPolicy{LossRate: 0.10, DupRate: 0.05, MaxDelay: 30 * time.Millisecond}
	relay := newChaosRelay(t, addr, seed, policy, policy)
	defer relay.Close()

	const clientID = uint16(9001)
	cli, err := NewClient(k, relay.Addr(), clientID, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatalf("seed=%d: server did not register client", seed)
	}

	// 出力: SSN 1..n を一定間隔で送る。ストリームは QUIC が再送するので、ロス下でも
	// 欠落・重複・順序崩れなく届くはず（tezzer 側は再送ロジックを持たない）。
	const n = 200
	want := make([]byte, 0, n*8)
	for i := 1; i <= n; i++ {
		want = append(want, []byte(fmt.Sprintf("out-%d\n", i))...)
	}
	go func() {
		for i := 1; i <= n; i++ {
			_ = srv.SendOutput(uint64(i), []byte(fmt.Sprintf("out-%d\n", i)), []transport.ClientID{{Num: clientID}})
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var got []byte
	deadline := time.After(20 * time.Second)
	for len(got) < len(want) {
		select {
		case chunk := <-cli.Output():
			got = append(got, chunk.Data...)
		case <-deadline:
			t.Fatalf("seed=%d: timeout waiting for output: got %d/%d bytes", seed, len(got), len(want))
		}
	}
	if string(got) != string(want) {
		t.Fatalf("seed=%d: output mismatch\n got=%q\nwant=%q", seed, got, want)
	}

	// 入力: DATAGRAM+ストリーム二重送信の dedup が、ロス下でも exactly-once で
	// 届くことを検証する（片方が落ちてももう片方が来る／両方来ても重複しない）。
	const m = 50
	wantIn := make([]byte, 0, m*8)
	for i := 0; i < m; i++ {
		wantIn = append(wantIn, []byte(fmt.Sprintf("in-%d;", i))...)
	}

	var gotIn []byte
	inCh := make(chan []byte, 256)
	go func() {
		for {
			select {
			case in, ok := <-srv.Input():
				if !ok {
					return
				}
				inCh <- in.Data
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for i := 0; i < m; i++ {
			data := []byte(fmt.Sprintf("in-%d;", i))
			if err := cli.SendInput(data); err != nil {
				t.Errorf("SendInput: %v", err)
				return
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()

	inDeadline := time.After(20 * time.Second)
	for len(gotIn) < len(wantIn) {
		select {
		case d := <-inCh:
			gotIn = append(gotIn, d...)
		case <-inDeadline:
			t.Fatalf("seed=%d: timeout waiting for input: got %d/%d bytes", seed, len(gotIn), len(wantIn))
		}
	}
	if string(gotIn) != string(wantIn) {
		t.Fatalf("seed=%d: input mismatch\n got=%q\nwant=%q", seed, gotIn, wantIn)
	}
}

// 注記: 「relay を MaxIdleTimeout(60s) 超で完全遮断し、watchdog の自然な connDead() 検知
// → 自動 reconnect → resync で復旧する」シナリオも実装したが、実測で全断からの検知が
// 60s の想定を大きく超えて遅延する（150s 以上待っても検知しないことがある）ケースが
// 再現し、待てば必ず終わる保証がないため削った。
