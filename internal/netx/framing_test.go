package netx

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func TestFramingRoundtrip(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{"empty", []byte{}},
		{"single byte", []byte{0x42}},
		{"small", []byte("hello world")},
		{"4KB", make([]byte, 4096)},
		{"64KB", make([]byte, 65535)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tt.payload); err != nil {
				t.Fatalf("WriteFrame failed: %v", err)
			}

			got, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame failed: %v", err)
			}

			if !bytes.Equal(got, tt.payload) {
				t.Errorf("roundtrip mismatch: got %d bytes, want %d bytes", len(got), len(tt.payload))
			}
		})
	}
}

func TestFramingRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		size := rng.Intn(10000)
		payload := make([]byte, size)
		rng.Read(payload)

		var buf bytes.Buffer
		if err := WriteFrame(&buf, payload); err != nil {
			t.Fatalf("WriteFrame failed: %v", err)
		}

		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame failed: %v", err)
		}

		if !bytes.Equal(got, payload) {
			t.Errorf("roundtrip mismatch: got %d bytes, want %d bytes", len(got), len(payload))
		}
	}
}

func TestReadFrameCorrupted(t *testing.T) {
	// Write a valid frame but truncate it
	var buf bytes.Buffer
	payload := []byte("hello")
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatalf("WriteFrame failed: %v", err)
	}

	// Truncate
	truncated := buf.Bytes()[:6] // only length + 2 bytes of payload
	r := bytes.NewReader(truncated)
	_, err := ReadFrame(r)
	if err != io.ErrUnexpectedEOF && err != io.EOF {
		t.Errorf("expected EOF error, got: %v", err)
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	// Write a fake length that exceeds maxFrameSize
	lenBuf := []byte{0xFF, 0xFF, 0xFF, 0xFF} // > 16MB
	buf.Write(lenBuf)

	_, err := ReadFrame(&buf)
	if err == nil {
		t.Error("expected error for oversized frame, got nil")
	}
}
