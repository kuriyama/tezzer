package session

// UDS の送信キューがあふれて出力を捨てたら、次に積めたとき出力より先に OUTPUT_DROPPED を
// 1 回だけ届けること。以前は捨てたチャンクごとに goroutine で通知を試み、キューが満杯なので
// 通知もほぼ確実に落ちていた。

import (
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/proto"
)

func TestHandlePTYOutput_DroppedNoteBeforeNextOutput(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()
	sess, err := mgr.CreateSession("", "/bin/cat", nil, nil, "", 24, 80, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()
	client := sess.AttachClient(0, "TEST", "slow", 0)
	deadline := time.Now().Add(2 * time.Second)
	for {
		sess.mu.RLock()
		catchingUp := client.catchingUp
		sess.mu.RUnlock()
		if !catchingUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attach catch-up did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	queue := cap(client.OutCh)
	for i := 0; i < queue+5; i++ { // 5 個はキューに入らず捨てられる
		sess.handlePTYOutput([]byte("x"))
	}
	for i := 0; i < queue; i++ {
		<-client.OutCh
	}
	sess.handlePTYOutput([]byte("after"))

	var kinds []string
	for len(client.OutCh) > 0 {
		msg, err := proto.Decode(<-client.OutCh)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		switch m := msg.(type) {
		case *proto.NoteMsg:
			kinds = append(kinds, m.Kind)
		case *proto.OutputMsg:
			kinds = append(kinds, "OUTPUT:"+string(m.Data))
		}
	}
	if len(kinds) != 2 || kinds[0] != "OUTPUT_DROPPED" || kinds[1] != "OUTPUT:after" {
		t.Errorf("queue after recovery = %q, want [OUTPUT_DROPPED OUTPUT:after]", kinds)
	}
}
