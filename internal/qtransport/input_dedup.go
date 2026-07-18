package qtransport

// inputDeduper は、入力ストリーム（信頼・順序保証）と入力 DATAGRAM（投機・二重送信）の
// どちらから届いたバイトも PTY へ一度だけ・offset 順に転送するための重複排除。
// 接続（＝入力ストリーム）単位で持ち、offset 0 から始まる。
//
// 方針: datagram は「適用位置にぴったり一致したときだけ」適用する。
// 先行しすぎた datagram（手前のストリームデータ未着）はバッファせず破棄し、
// ストリーム側の配送に任せる。dedup は正しさではなくレイテンシーのための機構なので
// シンプルさを優先する。
//
// 並行呼び出しは呼び出し側（serverClient.inMu）で直列化すること。
type inputDeduper struct {
	streamPos uint64 // 入力ストリームから読んだ累計バイト数
	applied   uint64 // PTY へ転送済みの累計バイト数
}

// fromStream はストリームから読んだ data のうち未転送の部分を返す（なければ nil）。
// 返り値は data のサブスライスなので、保持する場合は呼び出し側でコピーすること。
func (d *inputDeduper) fromStream(data []byte) []byte {
	start := d.streamPos
	d.streamPos += uint64(len(data))
	if start >= d.applied {
		// 全部未転送
		d.applied = d.streamPos
		return data
	}
	if d.streamPos <= d.applied {
		// 全部 datagram で転送済み
		return nil
	}
	// 前半は転送済み、後半が未転送
	skip := d.applied - start
	d.applied = d.streamPos
	return data[skip:]
}

// fromDatagram は datagram の offset が適用位置と一致する場合のみ data を返す。
// 重複（offset < applied）と先行しすぎ（offset > applied）は nil。
func (d *inputDeduper) fromDatagram(offset uint64, data []byte) []byte {
	if offset != d.applied || len(data) == 0 {
		return nil
	}
	d.applied += uint64(len(data))
	return data
}
