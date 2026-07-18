package stun

import (
	"encoding/binary"
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

// TestGetMappedAddr はSTUNサーバーとの実際のUDP往復（Client.GetMappedAddr）を
// テストします。外部ネットワークには依存せず、ループバック上に立てた最小の
// STUNサーバー（startLoopbackStunServer）を使うため、隔離環境でも常に実行される。
func TestGetMappedAddr(t *testing.T) {
	server := startLoopbackStunServer(t)

	client := NewClient(server)
	client.Timeout = 2 * time.Second

	addr, err := client.GetMappedAddr()
	if err != nil {
		t.Fatalf("failed to get mapped addr from %s: %v", server, err)
	}

	if addr.IP == nil {
		t.Errorf("got nil IP")
	}
	if addr.Port == 0 {
		t.Errorf("got port 0")
	}
}

// TestClientNetworkOverride は Client.Network を明示指定した場合に、
// その family でダイヤルされることを確認する（v4/v6 個別STUN問い合わせの土台）。
func TestClientNetworkOverride(t *testing.T) {
	server := startLoopbackStunServer(t)

	client := NewClient(server)
	client.Network = "udp4"
	client.Timeout = 2 * time.Second

	addr, err := client.GetMappedAddr()
	if err != nil {
		t.Fatalf("GetMappedAddr with Network=udp4 failed: %v", err)
	}
	if addr.IP.To4() == nil {
		t.Errorf("expected an IPv4 address, got %v", addr.IP)
	}
}

// TestProbe_EndpointIndependent は同一ソケット 2 サーバー比較の正常系。
// ループバック（NAT なし）では両サーバーから同じ mapped address が返り、
// EIM 判定・port 保存判定がともに true になる。
func TestProbe_EndpointIndependent(t *testing.T) {
	serverA := startLoopbackStunServer(t)
	serverB := startLoopbackStunServer(t)

	res, err := Probe("udp4", serverA, serverB, 2*time.Second)
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if !res.EndpointIndependent() {
		t.Errorf("expected endpoint-independent mapping on loopback: A=%v B=%v", res.MappedA, res.MappedB)
	}
	if !res.PortPreserving() {
		t.Errorf("expected port-preserving on loopback: local=%d mapped=%d", res.LocalPort, res.MappedA.Port)
	}
	if res.MappedA.Port != res.LocalPort {
		t.Errorf("mapped port %d != local port %d", res.MappedA.Port, res.LocalPort)
	}
}

// TestProbe_DestinationDependent は symmetric NAT 相当（サーバーごとに見える
// mapped port が異なる）を、port をずらして応答する偽サーバーで模擬し、
// EIM 判定が false になることを確認する。
func TestProbe_DestinationDependent(t *testing.T) {
	serverA := startLoopbackStunServer(t)
	serverB := startLoopbackStunServerWithPortOffset(t, 1)

	res, err := Probe("udp4", serverA, serverB, 2*time.Second)
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if res.EndpointIndependent() {
		t.Errorf("expected destination-dependent mapping: A=%v B=%v", res.MappedA, res.MappedB)
	}
}

// startLoopbackStunServer はテスト用の最小STUNサーバーをループバックに起動する。
// 受け取ったBinding RequestのTransaction IDと送信元アドレスをそのまま使い、
// XOR-MAPPED-ADDRESSにその送信元を詰めたBinding Success Responseを返すだけで、
// 認証やNAT越しの実挙動など本来のSTUNサーバーの複雑さは扱わない。
func startLoopbackStunServer(t *testing.T) string {
	return startLoopbackStunServerWithPortOffset(t, 0)
}

// startLoopbackStunServerWithPortOffset は XOR-MAPPED-ADDRESS の port に offset を
// 加えて応答する偽STUNサーバーを起動する。offset != 0 は「このサーバーから見える
// mapped port が他と違う」= symmetric NAT 相当の模擬に使う。
func startLoopbackStunServerWithPortOffset(t *testing.T, portOffset int) string {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("failed to start loopback STUN server: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, 1500)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // Cleanup がconnを閉じたら終了
			}
			if n < HeaderSize {
				continue
			}
			txID := append([]byte(nil), buf[8:20]...)
			mapped := &net.UDPAddr{IP: src.IP, Port: src.Port + portOffset}
			_, _ = conn.WriteToUDP(makeBindingSuccessResponse(txID, mapped), src)
		}
	}()

	return conn.LocalAddr().String()
}

// makeBindingSuccessResponse はXOR-MAPPED-ADDRESS属性1つだけを持つ
// Binding Success Response（IPv4限定）を組み立てる。makeBindingRequest /
// parseBindingResponse の鏡像。
func makeBindingSuccessResponse(txID []byte, addr *net.UDPAddr) []byte {
	ip4 := addr.IP.To4()

	attrValue := make([]byte, 8)
	attrValue[1] = 0x01 // Family: IPv4
	xport := uint16(addr.Port) ^ uint16(MagicCookie>>16)
	binary.BigEndian.PutUint16(attrValue[2:4], xport)
	xaddr := binary.BigEndian.Uint32(ip4) ^ MagicCookie
	binary.BigEndian.PutUint32(attrValue[4:8], xaddr)

	msgLen := 4 + len(attrValue) // attribute header + value
	msg := make([]byte, HeaderSize+msgLen)
	binary.BigEndian.PutUint16(msg[0:2], BindingResponse)
	binary.BigEndian.PutUint16(msg[2:4], uint16(msgLen))
	binary.BigEndian.PutUint32(msg[4:8], MagicCookie)
	copy(msg[8:20], txID)
	binary.BigEndian.PutUint16(msg[20:22], AttrXorMappedAddress)
	binary.BigEndian.PutUint16(msg[22:24], uint16(len(attrValue)))
	copy(msg[24:], attrValue)
	return msg
}
