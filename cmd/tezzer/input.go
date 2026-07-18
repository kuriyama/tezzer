package main

// input.go: stdin からの入力読み取りと送信バッチング。
// ESC シーケンス（カーソルキー等）は塊のまま、通常入力はペースト単位でまとめて
// 送信する状態機械（handleStdin）。エスケープキー（Ctrl-^ 等）のコマンド分岐も
// この読み取りループが担う。UTF-8 境界の分割回避（utf8IncompleteTrail）を含む。

import (
	"fmt"
	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
	"io"
	"log"
	"os"
	"time"
)

// sendInputBytes は入力バイト列をサーバーに送信する
func (c *Client) sendInputBytes(data []byte) error {
	// -peek: 入力は一切送らない（誤爆防止）。打鍵のたびに騒がないよう初回のみ通知
	if c.readOnly {
		if !c.roWarned {
			c.roWarned = true
			c.setStatusMessage("read-only attach (-peek): input is not sent")
		}
		return nil
	}
	if ct := c.transport(); ct != nil {
		return ct.SendInput(data)
	}

	// TCP経由で送信
	inputMsg := proto.InputMsg{
		Type:      "INPUT",
		SessionID: c.sessionID,
		Data:      data,
	}
	inputData, err := proto.Encode(inputMsg)
	if err != nil {
		return fmt.Errorf("encode input error: %w", err)
	}
	c.connMu.Lock()
	err = netx.WriteFrame(c.conn, inputData)
	c.connMu.Unlock()
	if err != nil {
		return fmt.Errorf("write input error: %w", err)
	}
	return nil
}

// flushEscBatch はESC batchingバッファをフラッシュして送信する
func (c *Client) flushEscBatch(reason string) {
	if !c.escBatchActive || len(c.escBatchBuf) == 0 {
		return
	}

	delay := time.Since(c.escBatchStart)

	// デバッグログ出力
	if c.isDebugEnabled() {
		log.Printf("INPUT: esc-batch flush reason=%s len=%d delay_ms=%.2f hex=%x",
			reason, len(c.escBatchBuf), float64(delay.Microseconds())/1000.0, c.escBatchBuf)
	}

	// 送信（sendInputBytes は同期的にエンコードして返るので元バッファをそのまま渡す）
	if err := c.sendInputBytes(c.escBatchBuf); err != nil {
		log.Printf("ESC batch send error: %v", err)
	}

	// 状態リセット
	c.escBatchActive = false
	c.escBatchBuf = nil
}

// utf8IncompleteTrail は data 末尾の不完全な UTF-8 シーケンスのバイト数を返す。
// 末尾が完全な UTF-8 文字で終わっている場合は 0 を返す。
func utf8IncompleteTrail(data []byte) int {
	n := len(data)
	if n == 0 {
		return 0
	}
	// 末尾から最大3バイトさかのぼってマルチバイト先頭バイトを探す
	// UTF-8 の先頭バイトは 0xC0 以上（continuation byte は 0x80-0xBF）
	limit := 3
	if limit > n {
		limit = n
	}
	for i := 1; i <= limit; i++ {
		b := data[n-i]
		if b < 0x80 {
			// ASCII: ここで完結しているので不完全なし
			return 0
		}
		if b >= 0xC0 {
			// マルチバイト先頭バイト発見
			// 期待される全体バイト数を求める
			var expected int
			if b < 0xE0 {
				expected = 2
			} else if b < 0xF0 {
				expected = 3
			} else {
				expected = 4
			}
			if i < expected {
				// 不完全: i バイトしかないが expected バイト必要
				return i
			}
			// 完全なシーケンス
			return 0
		}
		// 0x80-0xBF: continuation byte, さらにさかのぼる
	}
	// 4バイト以上 continuation byte が続く場合は壊れているので 0
	return 0
}

// flushInputBatch は通常入力batchingバッファをフラッシュして送信する
func (c *Client) flushInputBatch(reason string) {
	if len(c.inputBatchBuf) == 0 {
		return
	}

	sendBuf := c.inputBatchBuf

	// UTF-8 境界を意識して分割する（マルチバイト文字がパケット境界で分断されるのを防ぐ）
	var holdBack []byte
	trail := utf8IncompleteTrail(sendBuf)
	if trail > 0 {
		holdBack = make([]byte, trail)
		copy(holdBack, sendBuf[len(sendBuf)-trail:])
		sendBuf = sendBuf[:len(sendBuf)-trail]
	}

	delay := time.Since(c.inputBatchStart)
	bufLen := len(sendBuf)

	if bufLen > 0 {
		// デバッグログ出力
		if c.isDebugEnabled() {
			log.Printf("INPUT: batch flush reason=%s len=%d delay_ms=%.2f",
				reason, bufLen, float64(delay.Microseconds())/1000.0)
		}

		// 送信（sendInputBytes は同期的にエンコードして返るので元バッファをそのまま渡す）
		if err := c.sendInputBytes(sendBuf); err != nil {
			log.Printf("Input batch send error: %v", err)
		}
	}

	// 不完全な UTF-8 シーケンスがあれば次のバッチに持ち越す
	if len(holdBack) > 0 {
		c.inputBatchBuf = holdBack
		c.inputBatchStart = time.Now()
	} else {
		c.inputBatchBuf = nil
	}
}

// isCSIFinal は CSI シーケンスの終端バイトかどうかを判定する
// CSI の final byte は '~' または英字 (A-Z, a-z)
func isCSIFinal(b byte) bool {
	if b == '~' {
		return true
	}
	if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
		return true
	}
	return false
}

// shouldFlushEscBuf は ESC バッファを即座にフラッシュすべきかを判定する
// - ESC O A/B/C/D (application cursor keys): 即 flush
// - ESC [ A/B/C/D (CSI cursor keys): 即 flush
// - その他の CSI シーケンス: final byte まで待つ
func shouldFlushEscBuf(buf []byte) bool {
	bufLen := len(buf)

	// 最大バイト数に達した場合は強制 flush
	if bufLen >= 32 {
		return true
	}

	if bufLen < 3 {
		return false
	}

	if buf[0] != 0x1b {
		return false
	}

	// ESC O A/B/C/D (application cursor keys)
	if buf[1] == 'O' {
		switch buf[2] {
		case 'A', 'B', 'C', 'D':
			return true
		default:
			return false
		}
	}

	// ESC [ で始まる CSI シーケンス
	if buf[1] == '[' {
		// ESC [ A/B/C/D (cursor keys) は即 flush
		if bufLen == 3 {
			switch buf[2] {
			case 'A', 'B', 'C', 'D':
				return true
			}
		}
		// それ以外は final byte で判定
		if isCSIFinal(buf[bufLen-1]) {
			return true
		}
		return false
	}

	return false
}

// handleStdin reads from stdin and sends to server
func (c *Client) handleStdin() {
	// フラッシュタイマー（バッチ開始時のみアーム、アイドル時は停止）
	flushTimer := time.NewTimer(0)
	if !flushTimer.Stop() {
		<-flushTimer.C
	}
	defer flushTimer.Stop()

	resetFlushTimer := func(d time.Duration) {
		if !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
		flushTimer.Reset(d)
	}
	// バッチ状態に基づいてタイマーを更新する（バッチがなければ停止）
	recheckFlushTimer := func() {
		var d time.Duration
		if c.escBatchActive {
			d = 6*time.Millisecond - time.Since(c.escBatchStart)
		} else if len(c.inputBatchBuf) > 0 {
			d = 2*time.Millisecond - time.Since(c.inputBatchStart)
		} else {
			if !flushTimer.Stop() {
				select {
				case <-flushTimer.C:
				default:
				}
			}
			return
		}
		if d < 0 {
			d = 0
		}
		resetFlushTimer(d)
	}

	stdinCh := make(chan struct {
		data []byte
		n    int
		err  error
	}, 1)

	// stdin読み取りをgoroutineで実行
	go func() {
		// readBuf はこの goroutine 専用なのでループ外で 1 回だけ確保する。
		// チャネルへ渡す data は読み取りごとの新規コピー（受信側の処理と次の
		// Read が並行するため、readBuf をそのまま渡すことはできない）。
		readBuf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(readBuf)
			data := make([]byte, n)
			copy(data, readBuf[:n])
			stdinCh <- struct {
				data []byte
				n    int
				err  error
			}{data: data, n: n, err: err}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-flushTimer.C:
			if c.escBatchActive {
				c.flushEscBatch("timeout")
			}
			if len(c.inputBatchBuf) > 0 {
				c.flushInputBatch("timeout")
			}
			recheckFlushTimer()

		case result := <-stdinCh:
			n := result.n
			err := result.err
			data := result.data

			if err != nil {
				if err != io.EOF {
					log.Printf("stdin read error: %v\n", err)
				}
				c.doneOnce.Do(func() { close(c.done) })
				return
			}

			if n > 0 {
				// 1バイトずつ処理
				for i := 0; i < n; i++ {
					b := data[i]

					// エスケープキー（Ctrl-^ など）の処理
					if c.escapePressed {
						c.escapePressed = false
						if b == '.' {
							// エスケープキー + . でデタッチ（正常終了）
							fmt.Fprintf(os.Stderr, "\033[?25h")
							select {
							case c.errCh <- errDetached:
							default:
							}
							c.doneOnce.Do(func() { close(c.done) })
							time.Sleep(10 * time.Millisecond)
							c.conn.Close()
							fmt.Fprintf(os.Stderr, "\r\nDetached from session.\r\n")
							return
						}
						if b == 'i' {
							c.showStatus()
							continue
						}
						if b == 'h' {
							c.showHelp()
							continue
						}
						if b == 's' {
							go c.writeStatsFile()
							continue
						}
						if b == 'd' {
							c.toggleDebug()
							continue
						}
						if b == 'q' {
							// エスケープキー + q でセッション終了（-peek では無効）
							if c.readOnly {
								c.setStatusMessage("read-only attach (-peek): kill is disabled")
								continue
							}
							c.killSession()
							return
						}
						if b == 'r' {
							// エスケープキー + r でスクロール領域リセット＋画面リフレッシュ
							c.resetScrollRegion()
							continue
						}
						if b == 'f' {
							// エスケープキー + f で resize trick による強制再描画
							// （-peek では無効: 自分の端末サイズでリモート PTY を触ってしまうため）
							if c.readOnly {
								c.setStatusMessage("read-only attach (-peek): server redraw is disabled")
								continue
							}
							c.requestServerRedraw()
							continue
						}
						if b == c.escapeByte {
							// エスケープキー + エスケープキー でエスケープキーそのものを送信
							if err := c.sendInputBytes([]byte{b}); err != nil {
								log.Printf("send input error: %v", err)
								c.doneOnce.Do(func() { close(c.done) })
								return
							}
							continue
						}
						// その他のエスケープシーケンスは無視
						continue
					}

					if b == c.escapeByte {
						c.escapePressed = true
						continue
					}

					// ESC batching ロジック
					if b == 0x1b { // ESC
						// 既存のバッファがあれば先にフラッシュ
						if c.escBatchActive {
							c.flushEscBatch("new-esc")
						}
						// inputBatch が残っていれば先にフラッシュ（送信順序保証）
						// \e[201~（bracketed paste 終端）が content より先に届くのを防ぐ
						if len(c.inputBatchBuf) > 0 {
							c.flushInputBatch("pre-esc")
						}
						// 新しいESC batchingを開始
						c.escBatchActive = true
						c.escBatchBuf = []byte{0x1b}
						c.escBatchStart = time.Now()
						continue
					}

					if c.escBatchActive {
						// ESC batching中
						c.escBatchBuf = append(c.escBatchBuf, b)

						// 即座にフラッシュすべきか判定
						if shouldFlushEscBuf(c.escBatchBuf) {
							c.flushEscBatch("immediate")
						}
						continue
					}

					// 通常入力の一括処理: 次の ESC/escapeByte まで一括 append
					j := i + 1
					for j < n && data[j] != 0x1b && data[j] != c.escapeByte {
						j++
					}
					if len(c.inputBatchBuf) == 0 {
						c.inputBatchStart = time.Now()
					}
					c.inputBatchBuf = append(c.inputBatchBuf, data[i:j]...)
					if len(c.inputBatchBuf) >= 512 {
						c.flushInputBatch("size")
					}
					i = j - 1 // ループの i++ で j になる
				}
			}

			// 読み取り終了後、バッファにデータがあれば即座にフラッシュ
			// （ペースト全体を一度に送信するため）
			if len(c.inputBatchBuf) > 0 {
				c.flushInputBatch("read-end")
			}
			recheckFlushTimer()
		}
	}
}
