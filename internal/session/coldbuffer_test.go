package session

// 出力バッファ二層化（hot 生 + cold flate 圧縮）のテスト。
// 白箱で outputChunks / coldSegments を直接操作する（PTY 不要）。

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

// mkChunks は seq=start から n 個の連番チャンクを作る。
func mkChunks(start uint64, n int, ts time.Time) []OutputChunk {
	chunks := make([]OutputChunk, n)
	for i := range chunks {
		seq := start + uint64(i)
		chunks[i] = OutputChunk{
			Seq:       seq,
			Data:      []byte(fmt.Sprintf("chunk-%d: some terminal output line\r\n", seq)),
			Timestamp: ts,
		}
	}
	return chunks
}

// TestColdSegmentRoundtrip は圧縮 → 解凍で Seq とデータが完全に復元されることをテスト
func TestColdSegmentRoundtrip(t *testing.T) {
	chunks := mkChunks(100, 50, time.Now())
	seg, err := compressChunks(chunks)
	if err != nil {
		t.Fatalf("compressChunks: %v", err)
	}
	if seg.startSeq != 100 || seg.endSeq != 149 {
		t.Errorf("seq range = %d-%d, want 100-149", seg.startSeq, seg.endSeq)
	}

	got, err := seg.chunksFrom(0)
	if err != nil {
		t.Fatalf("chunksFrom: %v", err)
	}
	if len(got) != len(chunks) {
		t.Fatalf("got %d chunks, want %d", len(got), len(chunks))
	}
	for i, ch := range got {
		if ch.Seq != chunks[i].Seq || !bytes.Equal(ch.Data, chunks[i].Data) {
			t.Fatalf("chunk %d mismatch: seq=%d", i, ch.Seq)
		}
	}

	// fromSeq による絞り込み
	tail, err := seg.chunksFrom(140)
	if err != nil {
		t.Fatalf("chunksFrom(140): %v", err)
	}
	if len(tail) != 10 || tail[0].Seq != 140 {
		t.Fatalf("chunksFrom(140): got %d chunks starting at %d, want 10 starting at 140",
			len(tail), tail[0].Seq)
	}
}

// TestColdSegmentCompresses は端末出力様のデータで実際にサイズが縮むことをテスト
func TestColdSegmentCompresses(t *testing.T) {
	chunks := mkChunks(1, 1000, time.Now())
	raw := 0
	for _, ch := range chunks {
		raw += len(ch.Data)
	}
	seg, err := compressChunks(chunks)
	if err != nil {
		t.Fatalf("compressChunks: %v", err)
	}
	if seg.rawBytes != raw {
		t.Errorf("rawBytes = %d, want %d", seg.rawBytes, raw)
	}
	if len(seg.data) >= raw/2 {
		t.Errorf("compressed %d bytes from %d raw; expected at least 2x reduction", len(seg.data), raw)
	}
}

// TestHotOverflowToCold は hot 上限超過分が圧縮待ち経由で cold セグメントになることをテスト
func TestHotOverflowToCold(t *testing.T) {
	s := &Session{}
	now := time.Now()

	// hot 上限 4MB を大きく超えるチャンクを積む（1 チャンク 64KB × 100 = 6.4MB 相当）
	data := bytes.Repeat([]byte("terminal output pattern "), 64*1024/24)
	for i := 1; i <= 100; i++ {
		s.outputChunks = append(s.outputChunks, OutputChunk{Seq: uint64(i), Data: data, Timestamp: now})
		s.outputBufferBytes += len(data)
	}
	if !s.evictOutputChunksLocked(now) {
		t.Fatal("expected evict to request a cold flush (overflow >= coldSegmentRawTarget)")
	}
	s.flushColdPending()

	if s.outputBufferBytes > maxHotOutputBytes {
		t.Errorf("hot bytes %d exceeds cap %d", s.outputBufferBytes, maxHotOutputBytes)
	}
	overflow := s.coldPendingBytes + s.coldRawBytes
	if overflow == 0 {
		t.Fatal("expected overflow to move into cold tier, got none")
	}
	// あふれた分（約 2.4MB）は coldSegmentRawTarget(1MB) を超えるので一部は圧縮済みのはず
	if len(s.coldSegments) == 0 {
		t.Fatalf("expected at least one compressed segment (pending=%d bytes)", s.coldPendingBytes)
	}
	if s.coldBytes >= s.coldRawBytes {
		t.Errorf("cold compressed %d >= raw %d; compression had no effect", s.coldBytes, s.coldRawBytes)
	}
	// 全層合わせて欠損なし（hot 先頭の一つ前が cold/pending の末尾）
	oldestSeq, ok := s.oldestRetainedSeqLocked()
	if !ok || oldestSeq != 1 {
		t.Errorf("oldest retained seq = %d, want 1", oldestSeq)
	}
}

// TestOutputFromOffsetSpansTiers は再同期が cold + pending + hot をまたいで
// 欠損なく Seq 順に返すことをテスト
func TestOutputFromOffsetSpansTiers(t *testing.T) {
	s := &Session{}
	now := time.Now()

	// cold: seq 1-200 を 2 セグメントに
	seg1, err := compressChunks(mkChunks(1, 100, now))
	if err != nil {
		t.Fatal(err)
	}
	seg2, err := compressChunks(mkChunks(101, 100, now))
	if err != nil {
		t.Fatal(err)
	}
	s.coldSegments = []coldSegment{seg1, seg2}
	// pending: seq 201-210
	s.coldPending = mkChunks(201, 10, now)
	// hot: seq 211-220
	s.outputChunks = mkChunks(211, 10, now)

	// 再接続クライアント（fromOffset=150）: cold 途中から全層
	out, err := s.outputFromOffset(transport.ClientID{}, 150)
	if err != nil {
		t.Fatalf("outputFromOffset: %v", err)
	}
	if len(out) != 71 { // 150..220
		t.Fatalf("got %d chunks, want 71", len(out))
	}
	for i, ch := range out {
		if want := uint64(150 + i); ch.Offset != want {
			t.Fatalf("out[%d].Offset = %d, want %d (must be gapless and ordered)", i, ch.Offset, want)
		}
	}

	// 新規クライアント（fromOffset=1）: cold は再生せず raw 層のみ
	out, err = s.outputFromOffset(transport.ClientID{}, 1)
	if err != nil {
		t.Fatalf("outputFromOffset(1): %v", err)
	}
	if len(out) != 20 || out[0].Offset != 201 {
		t.Fatalf("fresh attach: got %d chunks starting at %d, want 20 starting at 201 (raw tiers only)",
			len(out), out[0].Offset)
	}
}

// TestColdEviction は cold 層が圧縮後サイズ上限・保持期限で evict されることをテスト
func TestColdEviction(t *testing.T) {
	s := &Session{}
	now := time.Now()

	// 期限切れセグメント + 新しいセグメント
	oldSeg, _ := compressChunks(mkChunks(1, 10, now.Add(-maxOutputBufferAge-time.Minute)))
	newSeg, _ := compressChunks(mkChunks(11, 10, now))
	s.coldSegments = []coldSegment{oldSeg, newSeg}
	s.coldBytes = len(oldSeg.data) + len(newSeg.data)
	s.coldRawBytes = oldSeg.rawBytes + newSeg.rawBytes

	s.evictOutputChunksLocked(now)

	if len(s.coldSegments) != 1 || s.coldSegments[0].startSeq != 11 {
		t.Fatalf("expected only the fresh segment to remain, got %d segments", len(s.coldSegments))
	}
	if s.coldBytes != len(newSeg.data) || s.coldRawBytes != newSeg.rawBytes {
		t.Errorf("cold byte counters not adjusted: bytes=%d raw=%d", s.coldBytes, s.coldRawBytes)
	}
}

// TestEvictStaleOutputFlushesPending は出力停止後の定期 evict で
// 圧縮待ちが cold セグメントへ flush されることをテスト
func TestEvictStaleOutputFlushesPending(t *testing.T) {
	s := &Session{}
	stale := time.Now().Add(-coldPendingFlushAge - time.Minute)
	s.coldPending = mkChunks(1, 10, stale)
	for _, ch := range s.coldPending {
		s.coldPendingBytes += len(ch.Data)
	}

	s.EvictStaleOutput()

	if len(s.coldPending) != 0 || s.coldPendingBytes != 0 {
		t.Fatalf("pending not flushed: %d chunks remain", len(s.coldPending))
	}
	if len(s.coldSegments) != 1 {
		t.Fatalf("expected 1 cold segment after flush, got %d", len(s.coldSegments))
	}
}

// TestEvictColdPendingSkippedDuringFlush は圧縮中（coldFlushing）は age evict が
// coldPending の先頭（in-flight バッチ）を触らないことをテスト。
// flushColdPending は完了時に「先頭 len(batch) 個 = 自分が切り出したバッチ」を
// 取り除く前提なので、圧縮中に先頭が別途 evict されるとズレて別チャンクを壊す。
func TestEvictColdPendingSkippedDuringFlush(t *testing.T) {
	s := &Session{}
	stale := time.Now().Add(-maxOutputBufferAge - time.Minute)
	s.coldPending = mkChunks(1, 5, stale)
	for _, ch := range s.coldPending {
		s.coldPendingBytes += len(ch.Data)
	}

	s.coldFlushing = true
	s.evictColdPendingLocked(time.Now())
	if len(s.coldPending) != 5 {
		t.Fatalf("pending evicted during flush: %d chunks remain, want 5", len(s.coldPending))
	}

	s.coldFlushing = false
	s.evictColdPendingLocked(time.Now())
	if len(s.coldPending) != 0 {
		t.Fatalf("stale pending not evicted after flush: %d chunks remain", len(s.coldPending))
	}
}

// TestFlushColdPendingConcurrentAppend は圧縮中の追記（handlePTYOutput 相当）と
// 競合しても、全チャンクが cold セグメント + pending のどちらかに欠損なく残ることを
// テスト（-race での実行が本命）。
func TestFlushColdPendingConcurrentAppend(t *testing.T) {
	s := &Session{}
	now := time.Now()
	s.coldPending = mkChunks(1, 100, now)
	for _, ch := range s.coldPending {
		s.coldPendingBytes += len(ch.Data)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, ch := range mkChunks(101, 100, now) {
			s.mu.Lock()
			s.coldPending = append(s.coldPending, ch)
			s.coldPendingBytes += len(ch.Data)
			s.mu.Unlock()
		}
	}()
	s.flushColdPending()
	<-done

	// スナップショットに入った分はセグメントへ、それ以降は pending に残る。
	// 境界は競合次第だが、全体として seq 1..200 が欠損なく引けること。
	out, err := s.outputFromOffset(transport.ClientID{}, 2)
	if err != nil {
		t.Fatalf("outputFromOffset: %v", err)
	}
	if len(out) != 199 {
		t.Fatalf("got %d chunks, want 199 (seq 2..200 gapless)", len(out))
	}
	for i, ch := range out {
		if want := uint64(2 + i); ch.Offset != want {
			t.Fatalf("out[%d].Offset = %d, want %d", i, ch.Offset, want)
		}
	}
	// バイトカウンタの整合: pending に残った分と一致する
	wantPending := 0
	for _, ch := range s.coldPending {
		wantPending += len(ch.Data)
	}
	if s.coldPendingBytes != wantPending {
		t.Errorf("coldPendingBytes = %d, want %d", s.coldPendingBytes, wantPending)
	}
}
