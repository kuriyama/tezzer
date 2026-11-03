package main

import (
	"errors"
	"testing"
)

// finish は最初の呼び出しの理由だけを残し、done を 1 回だけ閉じる。
func TestFinishFirstReasonWins(t *testing.T) {
	c := &Client{done: make(chan struct{})}
	first := errors.New("first")

	c.finish(first)
	c.finish(nil)                  // 後から正常終了が来ても上書きしない
	c.finish(errors.New("second")) // 二重 close で panic しない

	select {
	case <-c.done:
	default:
		t.Fatal("done not closed")
	}
	if c.exitErr != first {
		t.Errorf("exitErr = %v, want %v", c.exitErr, first)
	}
}

func TestFinishNilIsNormalExit(t *testing.T) {
	c := &Client{done: make(chan struct{})}
	c.finish(nil)
	c.finish(errors.New("late error"))
	if c.exitErr != nil {
		t.Errorf("exitErr = %v, want nil (normal exit decided first)", c.exitErr)
	}
}
