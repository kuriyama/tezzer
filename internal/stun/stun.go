// Package stun provides an RFC 5389 STUN client, used to discover the public
// IP/port across NAT.
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

// Client is a STUN client.
type Client struct {
	ServerAddr string
	Timeout    time.Duration
	IPv4Only   bool   // use IPv4 only (consulted only when Network is empty)
	Network    string // explicit family, "udp4"/"udp6"; empty selects "udp"/"udp4" from IPv4Only
}

// NewClient creates a new STUN client.
func NewClient(serverAddr string) *Client {
	return &Client{
		ServerAddr: serverAddr,
		Timeout:    5 * time.Second,
	}
}

// GetMappedAddr queries the STUN server and returns the public address as
// seen across NAT.
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

// ProbeResult is a NAT diagnosis obtained by querying two STUN servers from
// a single socket.
type ProbeResult struct {
	LocalPort int          // local port the queries were sent from
	MappedA   *net.UDPAddr // public address as seen by server A
	MappedB   *net.UDPAddr // public address as seen by server B (mapping comparison)
}

// EndpointIndependent reports whether the NAT mapping is
// endpoint-independent (EIM, the "cone" family). false means the mapping is
// destination-dependent (EDM, symmetric NAT) and a STUN-discovered address
// cannot be advertised to third parties. Finer cone subtypes (filtering
// behavior) would require an RFC 5780 server, so they are not distinguished.
func (r ProbeResult) EndpointIndependent() bool {
	return r.MappedA.IP.Equal(r.MappedB.IP) && r.MappedA.Port == r.MappedB.Port
}

// PortPreserving reports whether the NAT preserves the local port on the
// public side. tezzer's STUN candidate advertisement is the synthetic
// "public IP + QUIC listen port", which assumes port preservation; when this
// is false the advertised candidate will not match the actual mapping.
func (r ProbeResult) PortPreserving() bool {
	return r.MappedA.Port == r.LocalPort
}

// Probe sends Binding Requests to serverA and then serverB from a single UDP
// socket and returns each XOR-MAPPED-ADDRESS. Because both queries share one
// socket, comparing the results is a meaningful test of mapping
// destination-dependence (EIM/EDM = cone/symmetric); with separate sockets
// the local port changes on every query and the comparison is void (the
// removed DetectNATType made exactly that mistake and misreported almost any
// NAT as symmetric). network is "udp4" or "udp6". The diagnosis reflects the
// NAT device rather than any particular socket, so measuring with a
// throwaway socket generalizes.
func Probe(network, serverA, serverB string, timeout time.Duration) (ProbeResult, error) {
	conn, err := net.ListenUDP(network, nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("failed to bind probe socket: %w", err)
	}
	defer conn.Close()

	a, err := queryMappedFrom(conn, network, serverA, timeout)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("failed to query %s: %w", serverA, err)
	}
	b, err := queryMappedFrom(conn, network, serverB, timeout)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("failed to query %s: %w", serverB, err)
	}
	local := conn.LocalAddr().(*net.UDPAddr)
	return ProbeResult{LocalPort: local.Port, MappedA: a, MappedB: b}, nil
}

// queryMappedFrom は unconnected ソケット conn から server へ Binding Request を送り、
// トランザクション ID の一致する応答の XOR-MAPPED-ADDRESS を返す。
func queryMappedFrom(conn *net.UDPConn, network, server string, timeout time.Duration) (*net.UDPAddr, error) {
	raddr, err := net.ResolveUDPAddr(network, server)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve STUN server: %w", err)
	}

	txID := make([]byte, 12)
	if _, err := rand.Read(txID); err != nil {
		return nil, fmt.Errorf("failed to generate transaction ID: %w", err)
	}
	req := makeBindingRequest(txID)

	deadline := time.Now().Add(timeout)
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(req, raddr); err != nil {
		return nil, fmt.Errorf("failed to send STUN request: %w", err)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return nil, ErrTimeout
			}
			return nil, fmt.Errorf("failed to read STUN response: %w", err)
		}
		addr, err := parseBindingResponse(buf[:n], txID)
		if err != nil {
			continue // 別トランザクションの遅延応答・無関係パケットは読み飛ばす
		}
		return addr, nil
	}
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
