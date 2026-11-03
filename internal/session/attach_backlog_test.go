package session

// attach 直後に過去の出力を送っている最中にライブ出力が来ても、UDS の送信キューには
// 番号が飛ばず重複せず順番どおりに並ぶこと。以前は登録直後からライブ分が同じキューに
// 割り込み、クライアントは割り込んだ番号まで描画済みとみなして残りの過去分を捨てていた
// （過去分が送信キュー 1000 件を超えると、SSH へ送り出されるのを待つ数秒間ずっと起きうる）。

import (
	"fmt"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/proto"
)

func TestAttach_LiveOutputDuringBacklog(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()
	sess, err := mgr.CreateSession("", "/bin/cat", nil, nil, "", 24, 80, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()

	const backlog, live = 1500, 200
	for i := 1; i <= backlog; i++ {
		sess.handlePTYOutput([]byte(fmt.Sprintf("b%d\n", i)))
	}
	client := sess.AttachClient(0, "TEST", "attach", 0)
	go func() {
		for i := 1; i <= live; i++ {
			sess.handlePTYOutput([]byte(fmt.Sprintf("l%d\n", i)))
			time.Sleep(time.Millisecond)
		}
	}()

	var want uint64 = 1
	deadline := time.After(10 * time.Second)
	for want <= backlog+live {
		select {
		case raw := <-client.OutCh:
			msg, err := proto.Decode(raw)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			out, ok := msg.(*proto.OutputMsg)
			if !ok {
				continue
			}
			if out.Seq != want {
				t.Fatalf("got seq %d, want %d (live output overtook the backlog)", out.Seq, want)
			}
			want++
			time.Sleep(20 * time.Microsecond) // 送信キューが詰まった状態を保つ
		case <-deadline:
			t.Fatalf("timeout at seq %d", want)
		}
	}
}
