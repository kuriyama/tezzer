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

// ProbeResult は同一ソケットから 2 つの STUN サーバーへ問い合わせた NAT 診断結果。
type ProbeResult struct {
	LocalPort int          // 問い合わせに使ったローカルポート
	MappedA   *net.UDPAddr // サーバー A から見た公開アドレス
	MappedB   *net.UDPAddr // サーバー B から見た公開アドレス（マッピング比較用）
}

// EndpointIndependent は NAT マッピングが宛先非依存（EIM = cone 系）かを返す。
// false は宛先ごとにマッピングが変わる EDM（symmetric NAT）で、STUN で得た
// アドレスを第三者への広告に使えない。
// cone 系のさらなる細分（filtering 挙動）は RFC 5780 対応サーバーが必要なため扱わない。
func (r ProbeResult) EndpointIndependent() bool {
	return r.MappedA.IP.Equal(r.MappedB.IP) && r.MappedA.Port == r.MappedB.Port
}

// PortPreserving は NAT がローカルポートを公開側でも保存しているかを返す。
// tezzer の STUN 候補広告は「公開 IP + QUIC listen ポート」という port 保存前提の
// 合成値なので、これが false だと広告候補は実際のマッピングと一致しない。
func (r ProbeResult) PortPreserving() bool {
	return r.MappedA.Port == r.LocalPort
}

// Probe は 1 つの UDP ソケットから serverA / serverB へ順に Binding Request を送り、
// それぞれの XOR-MAPPED-ADDRESS を返す。同一ソケットからの比較なので、マッピングの
// 宛先依存性（EIM/EDM = cone/symmetric）の判定として意味を持つ。
// ソケットを分けるとローカルポートが毎回変わり、この比較は成立しない
// （旧 DetectNATType はこの誤りでほぼ常に symmetric と誤判定していた）。
// network は "udp4" / "udp6"。診断はソケット単位ではなく NAT 装置の性質を見るものなので、
// 使い捨てソケットで測って一般化してよい。
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
