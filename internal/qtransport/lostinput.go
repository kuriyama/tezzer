package qtransport

// lostinput.go: 届かなかった入力の記録（Client.inMu で保護）。
//
// 経路断の間の入力は送り直さない（閉じる直前に打った `rm` などが、数時間後の復帰時に
// 流れ込みうるため）。そのかわり、何が届かなかったかを OnInputLost で知らせる。
// 対象は次の 2 種類で、復帰時にまとめて 1 回知らせる。
//   - 送信に失敗して捨てた分（接続断の検知後、書き込み期限切れ）
//   - 書き込みは成功したが、届く前に接続が作り直されて消えた分。サーバが再接続時に
//     ctrlInputApplied で伝える「前の接続で PTY に渡した位置」と、送ったつもりの位置の差。

// sentKeep は届かなかった分を取り出すために手元に残す、直近に送った入力の量。
const sentKeep = 64 << 10

// lostHeadMax は届かなかった入力のうち、内容を保持する冒頭の量（残りはバイト数だけ数える）。
const lostHeadMax = 256

type lostInput struct {
	sent []byte // 今の入力ストリームに書いた直近 sentKeep バイト

	// stream は入力ストリームの通し番号（newStream ごとに増える）。サーバからの位置は
	// それを読んだ control ストリームの番号と合うときだけ使う（差し替え直後に旧 control で
	// 読めた位置を、新しい差し替えの位置と取り違えないため）。
	stream uint64

	// 再接続時に前の入力ストリームの状態を退避し、サーバからの位置（ctrlInputApplied）を待つ
	awaiting bool
	prevSent uint64 // 前のストリームに書いた累計バイト数
	prevTail []byte // 前のストリームに書いた直近 sentKeep バイト（末尾が prevSent に対応）

	total int    // まだ知らせていない届かなかった入力のバイト数
	head  []byte // その冒頭（最大 lostHeadMax）
	// headUnknown は届かなかった入力の先頭を手元に残していなかった（sentKeep を超えた）。
	// 途中から先を「冒頭」として見せないよう、知らせるまで内容は持たず数だけ数える。
	headUnknown bool
}

// recordSent は入力ストリームに書けた分を記録する。直近 sentKeep バイトがあればよいので、
// 打鍵ごとに詰め直さないよう 2 倍を超えたときだけ詰める。
func (l *lostInput) recordSent(b []byte) {
	l.sent = append(l.sent, b...)
	if len(l.sent) > 2*sentKeep {
		l.sent = append(l.sent[:0], l.sent[len(l.sent)-sentKeep:]...)
	}
}

// addLost は届かなかった入力を時系列の末尾に足す。
func (l *lostInput) addLost(b []byte) {
	l.total += len(b)
	if l.headUnknown {
		return
	}
	if room := lostHeadMax - len(l.head); room > 0 {
		l.head = append(l.head, b[:min(room, len(b))]...)
	}
}

// newStream は入力ストリームの差し替え（再接続・failover）で呼ぶ。sent は前のストリームに
// 書いた累計バイト数。前の差し替えの位置がまだ届いていない（awaiting）うちに再び差し替える
// と、その前のストリームの分は分からなくなる（サーバが伝えるのは直前の接続の位置だけ）。
// その場合は知らせない（誤報はしない）。新しいストリームの番号を返す。
func (l *lostInput) newStream(sent uint64) uint64 {
	tail := l.sent
	if len(tail) > sentKeep {
		tail = tail[len(tail)-sentKeep:]
	}
	l.awaiting = true
	l.prevSent = sent
	l.prevTail = tail
	l.sent = nil
	l.stream++
	return l.stream
}

// resolve はサーバからの位置（applied。nil = 不明）で前のストリームの届かなかった分を確定する。
// stream は位置を読んだ control ストリームと組の入力ストリームの番号で、今の番号と違えば
// 古い接続から遅れて届いたものとして捨てる。
// 前のストリームの分は、待っている間に送れず捨てた分より先に打たれているので前に置く。
func (l *lostInput) resolve(applied *uint64, stream uint64) {
	if !l.awaiting || stream != l.stream {
		return
	}
	l.awaiting = false
	if applied != nil && *applied < l.prevSent {
		missing := l.prevSent - *applied
		if missing > uint64(len(l.prevTail)) {
			// 届かなかった分の先頭を残していない。途中からを冒頭と見せないよう数だけにする
			l.total += int(missing)
			l.head = nil
			l.headUnknown = true
		} else {
			tail := l.prevTail[uint64(len(l.prevTail))-missing:]
			after := l.head
			l.head = nil
			l.addLost(tail)
			l.head = append(l.head, after[:min(lostHeadMax-len(l.head), len(after))]...)
		}
	}
	l.prevTail = nil
}

// take は知らせるべき届かなかった入力を取り出す（サーバからの位置を待っている間は出さない）。
func (l *lostInput) take() (int, []byte) {
	if l.awaiting || l.total == 0 {
		return 0, nil
	}
	total, head := l.total, l.head
	l.total, l.head, l.headUnknown = 0, nil, false
	return total, head
}
