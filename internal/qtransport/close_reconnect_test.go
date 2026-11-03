package qtransport

import (
	"context"
	"testing"

	"github.com/kuriyama/tezzer/internal/transport"
)

// Close 後の reconnect は候補を取り直さない（STUN 待ちで Close・終了が止まらないこと）。
func TestReconnect_NoRefreshAfterClose(t *testing.T) {
	refreshed := false
	cli := NewClient(make([]byte, 32), "192.0.2.1:4242", 1, "").(*quicClient)
	cli.SetCandidatesRefresher(func() []string {
		refreshed = true
		return []string{"192.0.2.2:4242"}
	})
	cli.ctx, cli.cancel = context.WithCancel(context.Background())
	cli.Close()

	if err := cli.reconnect("test"); err == nil {
		t.Fatal("reconnect after Close succeeded")
	}
	if refreshed {
		t.Error("reconnect after Close called RefreshCandidates")
	}
}

// Close 後は状態が Closed から戻らず、状態遷移も通知しない（終了後の "reconnecting" 表示を防ぐ）。
func TestSetStateIgnoredAfterClose(t *testing.T) {
	var msgs []string
	cli := NewClient(make([]byte, 32), "192.0.2.1:4242", 1, "").(*quicClient)
	cli.OnStateChange(func(_, _ transport.ConnectionState, msg string) {
		msgs = append(msgs, msg)
	})
	cli.Close()
	cli.setState(transport.StateRecovering, "reconnecting: test")
	if got := cli.State(); got != transport.StateClosed {
		t.Errorf("State() = %v, want StateClosed", got)
	}
	if len(msgs) != 1 || msgs[0] != "closed" {
		t.Errorf("state change notifications = %q, want [closed]", msgs)
	}
}
