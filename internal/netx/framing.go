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

	// Write length header (handle partial writes)
	if err := writeAll(w, lenBuf[:]); err != nil {
		return err
	}
	// Write payload (handle partial writes)
	if length > 0 {
		if err := writeAll(w, payload); err != nil {
			return err
		}
	}
	return nil
}

// writeAll writes all data to w, handling partial writes.
func writeAll(w io.Writer, data []byte) error {
	offset := 0
	for offset < len(data) {
		n, err := w.Write(data[offset:])
		if err != nil {
			return err
		}
		offset += n
	}
	return nil
}
