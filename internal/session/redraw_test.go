package session

import (
	"testing"
	"time"
)

// RequestRedraw の 50ms（列 - 1 に縮めている間）に本当のリサイズが来ても、取っておいた古い
// サイズで上書きしないこと。
func TestRequestRedrawKeepsConcurrentResize(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()
	sess, err := mgr.CreateSession("", "/bin/cat", nil, nil, "", 24, 80, false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	defer sess.Close()
	if err := sess.StartProcess(); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	done := make(chan struct{})
	go func() {
		sess.RequestRedraw()
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	if err := sess.Resize(30, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	<-done
	sess.mu.RLock()
	rows, cols := sess.Rows, sess.Cols
	sess.mu.RUnlock()
	if rows != 30 || cols != 100 {
		t.Errorf("size after RequestRedraw = %dx%d, want 30x100 (the resize made meanwhile)", rows, cols)
	}
}
