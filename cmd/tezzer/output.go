package main

// output.go: サーバ出力の受信と描画。
// UDS（handleServer。制御通知の配送も担う）と QUIC（handleQUICOutput）の両経路から
// 届く出力チャンクを、renderedSeq によるクロスパス重複排除（renderOutput）を通して
// 一度だけ stdout へ書く。

import (
	"fmt"
	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
	"github.com/kuriyama/tezzer/internal/transport"
	"io"
	"log"
	"os"
	"sync"
)

// handleQUICOutput は QUIC 経由の出力を処理し stdout に書く。
func (c *Client) handleQUICOutput() {
	ct := c.transport()
	if ct == nil {
		return
	}
	var firstOutput sync.Once
	for {
		select {
		case <-c.done:
			return
		case chunk, ok := <-ct.Output():
			if !ok {
				return
			}
			firstOutput.Do(func() {
				c.addLogMessage(fmt.Sprintf("QUIC: first server output received (%d bytes)", len(chunk.Data)))
			})
			// チャネルに溜まっている後続フレームを non-blocking でまとめ、
			// stdout への Write を 1 回にする（バースト出力時の syscall 削減）。
			// まとめすぎて書き出しが遅れないよう上限を設ける。
			const maxCoalesce = 256 * 1024
			chunks := []transport.OutputChunk{chunk}
			total := len(chunk.Data)
		drain:
			for total < maxCoalesce {
				select {
				case more, ok := <-ct.Output():
					if !ok {
						break drain
					}
					chunks = append(chunks, more)
					total += len(more.Data)
				default:
					break drain
				}
			}
			if c.isDebugEnabled() {
				log.Printf("QUIC: received output (%d bytes), writing to stdout\n", total)
			}
			c.renderOutput(chunks, true)
		}
	}
}

// handleServer reads from server and writes to stdout
func (c *Client) handleServer() {
	for {
		select {
		case <-c.done:
			// クリーンな切断（Ctrl-^ . など）
			return
		default:
		}

		// 接続を取得（ロックして即座に解放）
		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		frameData, err := netx.ReadFrame(conn)
		if err != nil {
			// doneが閉じている場合は、ユーザーが意図的に切断したのでメッセージ不要
			select {
			case <-c.done:
				return
			default:
			}

			// QUIC が有効な場合は制御チャネル（UDS）切断をログのみで続行
			if c.transport() != nil {
				if err != io.EOF {
					log.Printf("UDS control channel lost: %v", err)
				}
				c.setStatusMessage("[Tezzer] UDS control lost (QUIC continuing)")
				return
			}

			// QUIC 無効時はクライアント全体を異常終了
			select {
			case c.errCh <- fmt.Errorf("UDS read error: %w", err):
			default:
			}
			c.doneOnce.Do(func() { close(c.done) })
			return
		}

		msg, err := proto.Decode(frameData)
		if err != nil {
			log.Printf("decode error: %v", err)
			continue
		}

		switch m := msg.(type) {
		case *proto.OutputMsg:
			// 描画は renderOutput が renderedSeq で判定する（クロスパス重複排除）。
			// QUIC 接続中は通常サーバ側ゲーティングにより UDS 出力は届かないが、
			// 届いた場合（旧サーバ・ゲーティング切替の境界・QUIC 断中のフォール
			// バック）も一度だけ描画される。
			if c.transport() == nil {
				// UDS 単独経路のときだけ欠番を警告（QUIC 併用時は QUIC が信頼配送で埋める）
				lastSeq := c.lastSeq.Load()
				if lastSeq > 0 && m.Seq > lastSeq+1 {
					missing := m.Seq - lastSeq - 1
					c.setStatusMessage(fmt.Sprintf("UDS: %d messages lost (continuing)", missing))
				}
			}
			c.lastSeq.Store(m.Seq)

			// PTY出力をそのままstdoutに書く（OSC含む）
			c.renderOutput([]transport.OutputChunk{{Offset: m.Seq, Data: m.Data}}, false)

		case *proto.ErrorMsg:
			// セッション確立後に届く ERROR はサーバがセッションを継続できない状態
			// （QUIC_TIMEOUT でのセッション破棄等）。ログだけ出して居座ると、
			// ユーザーは終了済みセッションに attach したまま固まった画面を見続ける
			// ことになるため、メッセージを出してクリーンに終了する。
			errMsg := fmt.Errorf("server error: %s: %s", m.Code, m.Message)
			if m.Code == "QUIC_TIMEOUT" {
				errMsg = fmt.Errorf("%v\n(check that UDP between this host and the server is not blocked)", errMsg)
			}
			select {
			case c.errCh <- errMsg:
			default:
			}
			c.doneOnce.Do(func() { close(c.done) })
			return

		case *proto.NoteMsg:
			// OUTPUT_DROPPEDの場合はステータスで通知
			if m.Kind == "OUTPUT_DROPPED" {
				// QUIC 接続時は UDS の OutCh 詰まり由来の OUTPUT_DROPPED は誤検知（出力は
				// QUIC で信頼配送される）。真の欠損は再接続時に QUIC の ctrlStatus で届くので
				// ここでは無視する。
				if c.transport() != nil {
					continue
				}
				msg := "Output was dropped (use Ctrl-^ r to redraw)"
				if m.Msg != "" {
					msg = m.Msg
				}
				c.setStatusMessage(msg)
			} else if m.Kind == "SESSION_CLOSED" {
				// セッション終了時の通知（UDS 経由）
				// QUIC 側でも同通知が来うるため、CompareAndSwap で先着した側だけが表示する。
				if m.ExitCode != nil {
					c.noteSessionExitCode(*m.ExitCode)
				}
				if c.sessionClosedNotified.CompareAndSwap(false, true) {
					fmt.Fprintf(os.Stderr, "\r\n[Tezzer] Session closed: %s\r\n", m.Msg)
				}
				c.doneOnce.Do(func() { close(c.done) })
				return
			}

		case *proto.ServerMetaMsg:
			// サーバーメタ情報を受信
			c.metaMu.Lock()
			c.serverMeta = m
			c.metaMu.Unlock()

		default:
			// 未知のメッセージは無視
		}
	}
}
