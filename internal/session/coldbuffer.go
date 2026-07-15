package session

// 出力リングバッファの cold 層。
//
// 要件は「一晩〜数日スリープしたクライアントの再同期を違和感なく」であり、
// バッファが太るのは非対話シーン（誰も見ていない間に出力が流れ続けた）なので
// 性能は要らない。そこで直近の生チャンク（hot 層 = Session.outputChunks）から
// あふれた分を連結して flate 圧縮したセグメントとして保持する。端末出力は圧縮が
// よく効く（典型 5〜20 倍）ため、圧縮後の小さい上限で raw 換算数百 MB 相当・
// 数日分を保持できる。書き込み（圧縮）は hot あふれ時にまとめて、読み出し（解凍）は
// 古い offset からの再同期時のみの遅いパス。

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// coldChunkFrameLimit は解凍時のフレーム長サニティ上限。
// チャンクは PTY の 1 read（≤64KB）由来なので、これを超えるのは破損。
const coldChunkFrameLimit = 16 << 20

// coldSegment は hot 層からあふれたチャンク列を連結し flate 圧縮した 1 単位。
// data は [seq:u64be][len:u32be][bytes] フレームの繰り返しを圧縮したもの。
// 一度作られたら不変なので、参照を取ればロック外で解凍してよい。
type coldSegment struct {
	startSeq   uint64
	endSeq     uint64
	rawBytes   int       // 圧縮前の Data 合計（フレームヘッダ除く）
	data       []byte    // flate 圧縮済み
	oldestTime time.Time // 内部最古チャンクの Timestamp（統計用）
	newestTime time.Time // 内部最新チャンクの Timestamp（age evict 判定用）
}

// compressChunks は chunks（Seq 昇順であること）を 1 つの coldSegment に圧縮する。
func compressChunks(chunks []OutputChunk) (coldSegment, error) {
	if len(chunks) == 0 {
		return coldSegment{}, fmt.Errorf("compressChunks: empty input")
	}
	var buf bytes.Buffer
	// BestSpeed でも端末出力には十分効き、hot あふれ時の書き込みパスを重くしない
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		return coldSegment{}, err
	}
	var hdr [12]byte
	raw := 0
	for _, ch := range chunks {
		binary.BigEndian.PutUint64(hdr[0:8], ch.Seq)
		binary.BigEndian.PutUint32(hdr[8:12], uint32(len(ch.Data)))
		if _, err := w.Write(hdr[:]); err != nil {
			return coldSegment{}, err
		}
		if _, err := w.Write(ch.Data); err != nil {
			return coldSegment{}, err
		}
		raw += len(ch.Data)
	}
	if err := w.Close(); err != nil {
		return coldSegment{}, err
	}
	return coldSegment{
		startSeq:   chunks[0].Seq,
		endSeq:     chunks[len(chunks)-1].Seq,
		rawBytes:   raw,
		data:       buf.Bytes(),
		oldestTime: chunks[0].Timestamp,
		newestTime: chunks[len(chunks)-1].Timestamp,
	}, nil
}

// chunksFrom はセグメントを解凍して Seq >= fromSeq のチャンクを返す
// （再同期専用の遅いパス。Timestamp は復元しない）。
func (seg *coldSegment) chunksFrom(fromSeq uint64) ([]OutputChunk, error) {
	r := flate.NewReader(bytes.NewReader(seg.data))
	defer r.Close()
	var out []OutputChunk
	var hdr [12]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, fmt.Errorf("cold segment corrupt: %w", err)
		}
		seq := binary.BigEndian.Uint64(hdr[0:8])
		n := binary.BigEndian.Uint32(hdr[8:12])
		if n > coldChunkFrameLimit {
			return nil, fmt.Errorf("cold segment corrupt: frame too large (%d)", n)
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("cold segment corrupt: %w", err)
		}
		if seq >= fromSeq {
			out = append(out, OutputChunk{Seq: seq, Data: data})
		}
	}
}
