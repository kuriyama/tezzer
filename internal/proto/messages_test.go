package proto

import (
	"strings"
	"testing"
)

func TestEncodeDecodeHello(t *testing.T) {
	msg := &HelloMsg{
		Type:       "HELLO",
		V:          1,
		ClientName: "test-client",
		Cols:       80,
		Rows:       24,
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	decodedMsg, ok := decoded.(*HelloMsg)
	if !ok {
		t.Fatalf("expected *HelloMsg, got %T", decoded)
	}

	if decodedMsg.ClientName != msg.ClientName {
		t.Errorf("ClientName mismatch: got %s, want %s", decodedMsg.ClientName, msg.ClientName)
	}
	if decodedMsg.Cols != msg.Cols {
		t.Errorf("Cols mismatch: got %d, want %d", decodedMsg.Cols, msg.Cols)
	}
	if decodedMsg.Rows != msg.Rows {
		t.Errorf("Rows mismatch: got %d, want %d", decodedMsg.Rows, msg.Rows)
	}
}

func TestValidateSessionName(t *testing.T) {
	valid := []string{"work", "Work-1", "a", "dev.agent_2", "x-y_z.9"}
	for _, name := range valid {
		if err := ValidateSessionName(name); err != nil {
			t.Errorf("ValidateSessionName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"has space",
		"日本語",
		"slash/name",
		"colon:name",
		strings.Repeat("a", 64), // 長すぎ（64文字）
	}
	for _, name := range invalid {
		if err := ValidateSessionName(name); err == nil {
			t.Errorf("ValidateSessionName(%q) = nil, want error", name)
		}
	}
}

func TestEncodeDecodeCreateSession(t *testing.T) {
	msg := &CreateSessionMsg{
		Type: "CREATE_SESSION",
		Name: "work",
		Cmd:  "/bin/bash",
		Args: []string{"-l"},
		Env:  map[string]string{"TERM": "xterm-256color"},
		Cwd:  "/tmp",
		Cols: 80,
		Rows: 24,
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	decodedMsg, ok := decoded.(*CreateSessionMsg)
	if !ok {
		t.Fatalf("expected *CreateSessionMsg, got %T", decoded)
	}

	if decodedMsg.Cmd != msg.Cmd {
		t.Errorf("Cmd mismatch: got %s, want %s", decodedMsg.Cmd, msg.Cmd)
	}
	if decodedMsg.Name != msg.Name {
		t.Errorf("Name mismatch: got %s, want %s", decodedMsg.Name, msg.Name)
	}
	if len(decodedMsg.Args) != len(msg.Args) {
		t.Errorf("Args length mismatch: got %d, want %d", len(decodedMsg.Args), len(msg.Args))
	}
}

func TestEncodeDecodeError(t *testing.T) {
	msg := &ErrorMsg{
		Type:    "ERROR",
		Code:    ErrNoSuchSession,
		Message: "session not found",
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	decodedMsg, ok := decoded.(*ErrorMsg)
	if !ok {
		t.Fatalf("expected *ErrorMsg, got %T", decoded)
	}

	if decodedMsg.Code != msg.Code {
		t.Errorf("Code mismatch: got %s, want %s", decodedMsg.Code, msg.Code)
	}
	if decodedMsg.Message != msg.Message {
		t.Errorf("Message mismatch: got %s, want %s", decodedMsg.Message, msg.Message)
	}
}

func TestEncodeDecodeOutput(t *testing.T) {
	msg := &OutputMsg{
		Type:      "OUTPUT",
		SessionID: "test-session",
		Seq:       10,
		Data:      []byte("PTY output data"),
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	decodedMsg, ok := decoded.(*OutputMsg)
	if !ok {
		t.Fatalf("expected *OutputMsg, got %T", decoded)
	}

	if decodedMsg.SessionID != msg.SessionID {
		t.Errorf("SessionID mismatch: got %s, want %s", decodedMsg.SessionID, msg.SessionID)
	}
	if decodedMsg.Seq != msg.Seq {
		t.Errorf("Seq mismatch: got %d, want %d", decodedMsg.Seq, msg.Seq)
	}
	if string(decodedMsg.Data) != string(msg.Data) {
		t.Errorf("Data mismatch: got %s, want %s", string(decodedMsg.Data), string(msg.Data))
	}
}

func TestDecodeUnknownType(t *testing.T) {
	// 不明なメッセージタイプ
	msg := map[string]interface{}{
		"type": "UNKNOWN",
	}

	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}

	_, err = Decode(data)
	if err == nil {
		t.Fatal("expected error for unknown message type")
	}
}

func TestDecodeInvalidData(t *testing.T) {
	// 無効なデータ
	_, err := Decode([]byte("invalid msgpack data"))
	if err == nil {
		t.Fatal("expected error for invalid data")
	}
}
