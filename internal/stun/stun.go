// Package stun はRFC 5389準拠のSTUNクライアント実装を提供します。
// NAT越しの公開IP/ポートの取得に使用されます。
package stun

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	// STUN Message Types
	BindingRequest       = 0x0001
	BindingResponse      = 0x0101
	BindingErrorResponse = 0x0111

	// STUN Attribute Types
	AttrMappedAddress    = 0x0001
	AttrXorMappedAddress = 0x0020

	// STUN Magic Cookie (RFC 5389)
	MagicCookie = 0x2112A442

	// Header size
	HeaderSize = 20
)

var (
	ErrInvalidResponse = errors.New("invalid STUN response")
	ErrTimeout         = errors.New("STUN request timeout")
	ErrNoXorMapped     = errors.New("no XOR-MAPPED-ADDRESS in response")
)

// NATType はNATの種類を表します
type NATType int

const (
	NATTypeUnknown NATType = iota
	NATTypeFullCone
	NATTypeSymmetric
)

// Client はSTUNクライアントです。
type Client struct {
	ServerAddr string
	Timeout    time.Duration
	IPv4Only   bool   // IPv4のみ使用する場合true（Networkが空の場合のみ参照）
	Network    string // "udp4"/"udp6" で family を明示指定。空なら IPv4Only を見て "udp"/"udp4" を選ぶ
}

// NewClient は新しいSTUNクライアントを作成します。
func NewClient(serverAddr string) *Client {
	return &Client{
		ServerAddr: serverAddr,
		Timeout:    5 * time.Second,
	}
}

// GetMappedAddr はSTUNサーバーに問い合わせて、NAT越しの公開アドレスを取得します。
func (c *Client) GetMappedAddr() (*net.UDPAddr, error) {
	// UDP接続を作成（Network指定があれば優先、なければIPv4Onlyの場合にudp4を使用）
	network := "udp"
	if c.Network != "" {
		network = c.Network
	} else if c.IPv4Only {
		network = "udp4"
	}
	conn, err := net.DialTimeout(network, c.ServerAddr, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to STUN server: %w", err)
	}
	defer conn.Close()

	// Binding Requestを作成
	txID := make([]byte, 12)
	if _, err := rand.Read(txID); err != nil {
		return nil, fmt.Errorf("failed to generate transaction ID: %w", err)
	}

	req := makeBindingRequest(txID)

	// リクエストを送信
	if err := conn.SetWriteDeadline(time.Now().Add(c.Timeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("failed to send STUN request: %w", err)
	}

	// レスポンスを受信
	buf := make([]byte, 1500)
	if err := conn.SetReadDeadline(time.Now().Add(c.Timeout)); err != nil {
		return nil, err
	}
	n, err := conn.Read(buf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("failed to read STUN response: %w", err)
	}

	// レスポンスをパース
	return parseBindingResponse(buf[:n], txID)
}

// DetectNATType は複数のSTUNサーバーを使ってNATタイプを判別します。
// 異なるSTUNサーバーから同じ公開ポートが返されればCone NAT、
// 異なるポートが返されればSymmetric NATです。
func (c *Client) DetectNATType(serverA, serverB string) (NATType, *net.UDPAddr, error) {
	// サーバーAで公開アドレスを取得
	origServer := c.ServerAddr
	c.ServerAddr = serverA
	addrA, err := c.GetMappedAddr()
	if err != nil {
		c.ServerAddr = origServer
		return NATTypeUnknown, nil, fmt.Errorf("failed to query server A: %w", err)
	}

	// サーバーBで公開アドレスを取得
	c.ServerAddr = serverB
	addrB, err := c.GetMappedAddr()
	c.ServerAddr = origServer
	if err != nil {
		return NATTypeUnknown, addrA, fmt.Errorf("failed to query server B: %w", err)
	}

	// ポートが同じならCone NAT、異なればSymmetric NAT
	if addrA.Port == addrB.Port {
		return NATTypeFullCone, addrA, nil
	}
	return NATTypeSymmetric, addrA, nil
}

// makeBindingRequest はSTUN Binding Requestメッセージを作成します。
func makeBindingRequest(txID []byte) []byte {
	msg := make([]byte, HeaderSize)

	// Message Type (Binding Request)
	binary.BigEndian.PutUint16(msg[0:2], BindingRequest)

	// Message Length (ヘッダー以降のバイト数、今回は0)
	binary.BigEndian.PutUint16(msg[2:4], 0)

	// Magic Cookie
	binary.BigEndian.PutUint32(msg[4:8], MagicCookie)

	// Transaction ID (12 bytes)
	copy(msg[8:20], txID)

	return msg
}

// parseBindingResponse はSTUN Binding Responseをパースして、
// XOR-MAPPED-ADDRESSを抽出します。
func parseBindingResponse(data []byte, expectedTxID []byte) (*net.UDPAddr, error) {
	if len(data) < HeaderSize {
		return nil, ErrInvalidResponse
	}

	// Message Typeを確認
	msgType := binary.BigEndian.Uint16(data[0:2])
	if msgType != BindingResponse {
		return nil, fmt.Errorf("unexpected message type: 0x%04x", msgType)
	}

	// Message Lengthを取得
	msgLen := binary.BigEndian.Uint16(data[2:4])
	if len(data) < HeaderSize+int(msgLen) {
		return nil, ErrInvalidResponse
	}

	// Magic Cookieを確認
	cookie := binary.BigEndian.Uint32(data[4:8])
	if cookie != MagicCookie {
		return nil, fmt.Errorf("invalid magic cookie: 0x%08x", cookie)
	}

	// Transaction IDを確認
	txID := data[8:20]
	for i := 0; i < 12; i++ {
		if txID[i] != expectedTxID[i] {
			return nil, fmt.Errorf("transaction ID mismatch")
		}
	}

	// Attributesをパース
	offset := HeaderSize
	for offset < HeaderSize+int(msgLen) {
		if offset+4 > len(data) {
			break
		}

		attrType := binary.BigEndian.Uint16(data[offset : offset+2])
		attrLen := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		offset += 4

		if offset+int(attrLen) > len(data) {
			break
		}

		attrValue := data[offset : offset+int(attrLen)]

		// XOR-MAPPED-ADDRESSを探す
		if attrType == AttrXorMappedAddress {
			addr, err := parseXorMappedAddress(attrValue, txID)
			if err != nil {
				return nil, err
			}
			return addr, nil
		}

		// 4バイト境界にアライン
		offset += int(attrLen)
		if attrLen%4 != 0 {
			offset += 4 - int(attrLen%4)
		}
	}

	return nil, ErrNoXorMapped
}

// parseXorMappedAddress はXOR-MAPPED-ADDRESS属性をパースします。
func parseXorMappedAddress(data []byte, txID []byte) (*net.UDPAddr, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("XOR-MAPPED-ADDRESS too short")
	}

	// Family (0x01 = IPv4, 0x02 = IPv6)
	family := data[1]

	// X-Port (XORed with most significant 16 bits of magic cookie)
	xport := binary.BigEndian.Uint16(data[2:4])
	port := int(xport ^ uint16(MagicCookie>>16))

	var ip net.IP

	if family == 0x01 {
		// IPv4
		if len(data) < 8 {
			return nil, fmt.Errorf("XOR-MAPPED-ADDRESS IPv4 too short")
		}

		xaddr := binary.BigEndian.Uint32(data[4:8])
		addr := xaddr ^ MagicCookie

		ip = net.IPv4(
			byte(addr>>24),
			byte(addr>>16),
			byte(addr>>8),
			byte(addr),
		)
	} else if family == 0x02 {
		// IPv6
		if len(data) < 20 {
			return nil, fmt.Errorf("XOR-MAPPED-ADDRESS IPv6 too short")
		}

		xaddr := data[4:20]
		ip = make(net.IP, 16)

		// XOR with magic cookie (first 4 bytes)
		cookieBytes := []byte{
			byte((MagicCookie >> 24) & 0xFF),
			byte((MagicCookie >> 16) & 0xFF),
			byte((MagicCookie >> 8) & 0xFF),
			byte(MagicCookie & 0xFF),
		}
		for i := 0; i < 4; i++ {
			ip[i] = xaddr[i] ^ cookieBytes[i]
		}

		// XOR with transaction ID (remaining 12 bytes)
		for i := 0; i < 12; i++ {
			ip[4+i] = xaddr[4+i] ^ txID[i]
		}
	} else {
		return nil, fmt.Errorf("unsupported address family: 0x%02x", family)
	}

	return &net.UDPAddr{
		IP:   ip,
		Port: port,
	}, nil
}
