// Package termui provides terminal UI utilities for tezzer client:
// status line display (TTY direct, 3秒後に自動消去) and raw-mode CRLF translation.
package termui

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// CRLFWriter はターミナルがraw mode時に\nを\r\nに変換するWriter
type CRLFWriter struct {
	W io.Writer
}

func (c *CRLFWriter) Write(p []byte) (n int, err error) {
	// \n を \r\n に置換
	replaced := bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
	_, err = c.W.Write(replaced)
	if err != nil {
		return 0, err
	}
	return len(p), nil // 元のバイト数を返す
}

// StatusManager はステータス行の表示を管理する（TTYへの直接表示、3秒後に自動消去）。
type StatusManager struct {
	mu        sync.Mutex
	timer     *time.Timer
	displayFn func(msg string) // 表示用コールバック
	clearFn   func()           // 表示クリア用コールバック
}

// NewStatusManager はStatusManagerを作成する
func NewStatusManager(displayFn func(msg string), clearFn func()) *StatusManager {
	return &StatusManager{
		displayFn: displayFn,
		clearFn:   clearFn,
	}
}

// Close はStatusManagerを終了する（保留中の自動消去タイマーを止める）。
func (sm *StatusManager) Close() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.timer != nil {
		sm.timer.Stop()
	}
}

// Set はステータスメッセージを表示し、3秒後に自動で消去する。
func (sm *StatusManager) Set(msg string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.timer != nil {
		sm.timer.Stop()
	}
	if sm.displayFn != nil {
		sm.displayFn(msg)
	}

	sm.timer = time.AfterFunc(3*time.Second, func() {
		sm.mu.Lock()
		defer sm.mu.Unlock()
		if sm.clearFn != nil {
			sm.clearFn()
		}
	})
}
