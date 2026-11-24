package qtransport

// 候補アドレスの受け取り方の検証: 空なら Start が明示的なエラーを返すこと、
// 呼び出し側のスライスを後で書き換えても transport 側の候補に影響しないこと。

import (
	"context"
	"strings"
	"testing"
)

func TestClientStart_NoCandidates(t *testing.T) {
	cli := NewClient(ClientConfig{Key: make([]byte, 32)})
	defer cli.Close()
	err := cli.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no candidate") {
		t.Fatalf("Start with no candidates: err = %v, want a no-candidate error", err)
	}
}

func TestClient_CandidatesAreCopied(t *testing.T) {
	cands := []string{"192.0.2.1:4242"}
	cli := NewClient(ClientConfig{Key: make([]byte, 32), Candidates: cands})
	defer cli.Close()
	cands[0] = "mutated"
	cli.candidatesMu.Lock()
	got := cli.candidates
	cli.candidatesMu.Unlock()
	if len(got) != 1 || got[0] != "192.0.2.1:4242" {
		t.Fatalf("candidates after NewClient = %v, want [192.0.2.1:4242]", got)
	}

	roaming := []string{"192.0.2.2:4242"}
	cli.SetRoamingCandidates(roaming)
	roaming[0] = "mutated"

	cli.candidatesMu.Lock()
	got = cli.candidates
	cli.candidatesMu.Unlock()
	if len(got) != 1 || got[0] != "192.0.2.2:4242" {
		t.Fatalf("candidates = %v, want [192.0.2.2:4242]", got)
	}
}

// Close 後の reconnect は候補を取り直さない（STUN 待ちで Close・終了が止まらないこと）。
func TestReconnect_NoRefreshAfterClose(t *testing.T) {
	refreshed := false
	cli := NewClient(ClientConfig{
		Key:        make([]byte, 32),
		Candidates: []string{"192.0.2.1:4242"},
		RefreshCandidates: func() []string {
			refreshed = true
			return []string{"192.0.2.2:4242"}
		},
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
	cli := NewClient(ClientConfig{
		Key:        make([]byte, 32),
		Candidates: []string{"192.0.2.1:4242"},
		OnStateChange: func(_, _ ConnectionState, msg string) {
			msgs = append(msgs, msg)
		},
	})
	cli.Close()
	cli.setState(StateRecovering, "reconnecting: test")
	if got := cli.State(); got != StateClosed {
		t.Errorf("State() = %v, want StateClosed", got)
	}
	if len(msgs) != 1 || msgs[0] != "closed" {
		t.Errorf("state change notifications = %q, want [closed]", msgs)
	}
}
