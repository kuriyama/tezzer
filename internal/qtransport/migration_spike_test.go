package qtransport

// Spike B: connection migration（ローミング）でストリーム状態が保持されるか。
//
// 検証内容:
//   - クライアントが path1（ローカル socket A）で QUIC 接続＋ストリーム往復
//   - 新しい Transport（ローカル socket B = rebind 相当）を AddPath → Probe → Switch
//   - 移行後も「同一ストリーム」でデータが継続（offset 連続・欠落なし）すること
//   - クライアントの LocalAddr が新 socket に変わっていること（実際に経路が動いた証拠）
//
// 方針（前ターンで合意）: rebind は loopback 上の別ローカル UDP socket への能動 migration
// で模擬する。旧パスを完全に殺す faithfulness は実機 e2e（Spike D 相当）の領分。

import (
	"bufio"
	"context"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestSpikeB_Migration_StreamSurvives(t *testing.T) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}

	// --- サーバ: 接続を1本受けて、ストリームを行単位でエコーする ---
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLSForKey(t, k, k), nil)
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	srvRemote := make(chan string, 4) // サーバから見たクライアントアドレスの記録
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			srvErr <- err
			return
		}
		str, err := conn.AcceptStream(ctx)
		if err != nil {
			srvErr <- err
			return
		}
		br := bufio.NewReader(str)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			// このパケットを受けたときのクライアントアドレスを記録
			select {
			case srvRemote <- conn.RemoteAddr().String():
			default:
			}
			if _, err := str.Write([]byte(line)); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// --- クライアント path1: 明示 Transport で Dial（後で AddPath するため）---
	uconn1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP path1: %v", err)
	}
	tr1 := &quic.Transport{Conn: uconn1}
	defer tr1.Close()

	conn, err := tr1.Dial(ctx, ln.Addr(), clientTLSForKey(t, k, k), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.CloseWithError(0, "done")

	str, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	cbr := bufio.NewReader(str)

	roundTrip := func(msg string) {
		t.Helper()
		if _, err := str.Write([]byte(msg + "\n")); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		line, err := cbr.ReadString('\n')
		if err != nil {
			t.Fatalf("read echo of %q: %v", msg, err)
		}
		if got := line[:len(line)-1]; got != msg {
			t.Fatalf("echo mismatch: got %q want %q", got, msg)
		}
	}

	// path1 で往復
	roundTrip("msg-1-before-migration")
	localBefore := conn.LocalAddr().String()

	// --- path2 を追加して migration（rebind 相当）---
	uconn2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP path2: %v", err)
	}
	tr2 := &quic.Transport{Conn: uconn2}
	defer tr2.Close()

	path, err := conn.AddPath(tr2)
	if err != nil {
		t.Fatalf("AddPath: %v", err)
	}
	if err := path.Probe(ctx); err != nil {
		t.Fatalf("Probe (path validation): %v", err)
	}
	if err := path.Switch(); err != nil {
		t.Fatalf("Switch: %v", err)
	}

	// --- 移行後: 同一ストリームで往復が継続すること ---
	roundTrip("msg-2-after-migration")
	roundTrip("msg-3-after-migration")
	localAfter := conn.LocalAddr().String()

	if localAfter == localBefore {
		t.Fatalf("LocalAddr did not change after migration (before=%s after=%s)", localBefore, localAfter)
	}
	if localAfter != uconn2.LocalAddr().String() {
		t.Fatalf("active path is not the new socket: LocalAddr=%s want=%s", localAfter, uconn2.LocalAddr().String())
	}
	t.Logf("migration OK: client local %s -> %s, same stream carried msg-1/2/3", localBefore, localAfter)

	// サーバ側もクライアントアドレスの変化を観測しているはず
	seen := map[string]bool{}
drain:
	for {
		select {
		case a := <-srvRemote:
			seen[a] = true
		default:
			break drain
		}
	}
	if len(seen) < 2 {
		t.Logf("note: server observed client addrs=%v (migration may have reused validation path)", seen)
	} else {
		t.Logf("server observed client address change: %v", seen)
	}

	select {
	case err := <-srvErr:
		t.Fatalf("server side error: %v", err)
	default:
	}
}
