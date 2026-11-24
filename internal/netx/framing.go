package netx

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ReadFrame reads a length-prefixed frame from r.
// Format: [u32be length][payload]
func ReadFrame(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lenBuf[:])

	// Sanity check to prevent allocation attacks
	const maxFrameSize = 16 * 1024 * 1024 // 16MB
	if length > maxFrameSize {
		return nil, fmt.Errorf("frame too large: %d bytes", length)
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

// WriteFrame writes a length-prefixed frame to w.
// Format: [u32be length][payload]
func WriteFrame(w io.Writer, payload []byte) error {
	length := uint32(len(payload))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], length)

	// io.Writer は書き切れなければ必ずエラーを返す契約なので、部分書き込みの再試行は不要
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	if length > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}
