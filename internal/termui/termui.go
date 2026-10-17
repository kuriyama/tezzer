// Package termui provides terminal UI utilities for tezzer client:
// status line display (written directly to the TTY and cleared after 3 seconds)
// and raw-mode CRLF translation.
package termui

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// CRLFWriter is a Writer that translates \n to \r\n while the terminal is in raw mode.
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

// StatusManager manages the status line, written directly to the TTY and
// cleared automatically after 3 seconds.
type StatusManager struct {
	mu        sync.Mutex
	timer     *time.Timer
	displayFn func(msg string) // 表示用コールバック
	clearFn   func()           // 表示クリア用コールバック
}

// NewStatusManager returns a StatusManager that draws with displayFn and clears
// with clearFn.
func NewStatusManager(displayFn func(msg string), clearFn func()) *StatusManager {
	return &StatusManager{
		displayFn: displayFn,
		clearFn:   clearFn,
	}
}

// Close stops the StatusManager, cancelling any pending auto-clear timer.
func (sm *StatusManager) Close() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.timer != nil {
		sm.timer.Stop()
	}
}

// Set displays msg and clears it automatically after 3 seconds.
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
