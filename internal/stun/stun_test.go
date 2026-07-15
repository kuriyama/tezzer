package stun

import (
	"net"
	"testing"
	"time"
)

// TestNewClient はクライアント作成をテストします。
func TestNewClient(t *testing.T) {
	client := NewClient("stun.l.google.com:19302")
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	if client.ServerAddr != "stun.l.google.com:19302" {
		t.Errorf("unexpected server addr: %s", client.ServerAddr)
	}
	if client.Timeout != 5*time.Second {
		t.Errorf("unexpected timeout: %v", client.Timeout)
	}
}

// TestMakeBindingRequest はBinding Requestの作成をテストします。
func TestMakeBindingRequest(t *testing.T) {
	txID := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	req := makeBindingRequest(txID)

	if len(req) != HeaderSize {
		t.Fatalf("unexpected request size: %d", len(req))
	}

	// Message Type
	msgType := uint16(req[0])<<8 | uint16(req[1])
	if msgType != BindingRequest {
		t.Errorf("unexpected message type: 0x%04x", msgType)
	}

	// Message Length
	msgLen := uint16(req[2])<<8 | uint16(req[3])
	if msgLen != 0 {
		t.Errorf("unexpected message length: %d", msgLen)
	}

	// Magic Cookie
	cookie := uint32(req[4])<<24 | uint32(req[5])<<16 | uint32(req[6])<<8 | uint32(req[7])
	if cookie != MagicCookie {
		t.Errorf("unexpected magic cookie: 0x%08x", cookie)
	}

	// Transaction ID
	for i := 0; i < 12; i++ {
		if req[8+i] != txID[i] {
			t.Errorf("transaction ID mismatch at byte %d", i)
		}
	}
}

// TestParseXorMappedAddress はXOR-MAPPED-ADDRESSのパースをテストします。
func TestParseXorMappedAddress(t *testing.T) {
	txID := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}

	// IPv4の例: 192.0.2.1:32853
	// X-Port = 32853 XOR 0x2112 = 32853 XOR 8466 = 41371 (0xA1AB)
	// X-Address = 192.0.2.1 XOR 0x2112A442
	xport := uint16(32853 ^ (MagicCookie >> 16))
	xaddr := uint32(0xC0000201) ^ MagicCookie

	data := make([]byte, 8)
	data[0] = 0    // Reserved
	data[1] = 0x01 // Family: IPv4
	data[2] = byte(xport >> 8)
	data[3] = byte(xport)
	data[4] = byte(xaddr >> 24)
	data[5] = byte(xaddr >> 16)
	data[6] = byte(xaddr >> 8)
	data[7] = byte(xaddr)

	addr, err := parseXorMappedAddress(data, txID)
	if err != nil {
		t.Fatalf("parseXorMappedAddress failed: %v", err)
	}

	expectedIP := net.IPv4(192, 0, 2, 1)
	if !addr.IP.Equal(expectedIP) {
		t.Errorf("unexpected IP: got %v, want %v", addr.IP, expectedIP)
	}

	if addr.Port != 32853 {
		t.Errorf("unexpected port: got %d, want 32853", addr.Port)
	}
}

// TestGetMappedAddr は実際のSTUNサーバーへの接続をテストします。
// ネットワーク接続が必要なため、環境によっては失敗する可能性があります。
func TestGetMappedAddr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	// Google Public STUN serverを使用
	servers := []string{
		"stun.l.google.com:19302",
		"stun1.l.google.com:19302",
	}

	var lastErr error
	for _, server := range servers {
		client := NewClient(server)
		client.Timeout = 10 * time.Second

		addr, err := client.GetMappedAddr()
		if err != nil {
			lastErr = err
			t.Logf("failed to get mapped addr from %s: %v", server, err)
			continue
		}

		t.Logf("successfully got mapped address from %s: %v", server, addr)

		if addr.IP == nil {
			t.Errorf("got nil IP")
		}
		if addr.Port == 0 {
			t.Errorf("got port 0")
		}

		// 成功したら終了
		return
	}

	// すべてのサーバーで失敗した場合
	t.Errorf("failed to get mapped addr from all servers, last error: %v", lastErr)
}
