package main

// resolveCandidates の STUN 問い合わせ側の検証。ループバックに最小の STUN 応答器を立て、
// 外部ネットワークに依存せずに「サーバ指定の STUN を使う」「結果をリモート判定に使う」
// 「STUNAddrs がなければ問い合わせない」を確かめる。

import (
	"encoding/binary"
	"net"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/kuriyama/tezzer/internal/stun"
)

// startLoopbackSTUN は Binding Request に送信元アドレスを XOR-MAPPED-ADDRESS で返すだけの
// 最小 STUN サーバ（IPv4 のみ）を起動し、アドレスと受信リクエスト数を返す。
func startLoopbackSTUN(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	var requests atomic.Int32
	go func() {
		buf := make([]byte, 1500)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < stun.HeaderSize {
				continue
			}
			requests.Add(1)
			resp := make([]byte, stun.HeaderSize+12)
			binary.BigEndian.PutUint16(resp[0:2], stun.BindingResponse)
			binary.BigEndian.PutUint16(resp[2:4], 12)
			binary.BigEndian.PutUint32(resp[4:8], stun.MagicCookie)
			copy(resp[8:20], buf[8:20]) // transaction ID
			binary.BigEndian.PutUint16(resp[20:22], stun.AttrXorMappedAddress)
			binary.BigEndian.PutUint16(resp[22:24], 8)
			resp[25] = 0x01 // IPv4
			binary.BigEndian.PutUint16(resp[26:28], uint16(src.Port)^uint16(stun.MagicCookie>>16))
			binary.BigEndian.PutUint32(resp[28:32], binary.BigEndian.Uint32(src.IP.To4())^stun.MagicCookie)
			_, _ = conn.WriteToUDP(resp, src)
		}
	}()
	return conn.LocalAddr().String(), &requests
}

func TestResolveCandidates_UsesServerSTUN(t *testing.T) {
	server, requests := startLoopbackSTUN(t)
	c := &Client{ipv4Only: true}
	info := &QUICInfo{
		Port:       4242,
		LocalAddrs: []string{"10.0.0.5:4242"},
		// サーバの公開アドレスがクライアントの STUN 結果（127.0.0.1）と一致する = 同一ネットワーク
		STUNAddrs:  []string{"127.0.0.1:4242"},
		STUNServer: server,
	}

	cands, clientSTUN := c.resolveCandidates(info, false)

	if requests.Load() == 0 {
		t.Fatal("STUN server given in QUICInfo was not queried")
	}
	if len(clientSTUN) != 1 || hostOf(clientSTUN[0]) != "127.0.0.1" {
		t.Fatalf("clientSTUN = %v, want one 127.0.0.1 address", clientSTUN)
	}
	// 同一ネットワーク判定になるので LAN 候補も含まれる
	if !slices.Contains(cands, "10.0.0.5:4242") || !slices.Contains(cands, "127.0.0.1:4242") {
		t.Errorf("cands = %v, want LAN and STUN candidates", cands)
	}
}

func TestResolveCandidates_NoServerSTUNAddrs(t *testing.T) {
	server, requests := startLoopbackSTUN(t)
	c := &Client{ipv4Only: true}
	info := &QUICInfo{Port: 4242, LocalAddrs: []string{"10.0.0.5:4242"}, STUNServer: server}

	cands, clientSTUN := c.resolveCandidates(info, false)

	if requests.Load() != 0 || clientSTUN != nil {
		t.Fatalf("queried STUN without server STUNAddrs (requests=%d clientSTUN=%v)", requests.Load(), clientSTUN)
	}
	if !slices.Equal(cands, buildQUICCandidateAddrs(info, nil)) {
		t.Errorf("cands = %v, want buildQUICCandidateAddrs(info, nil)", cands)
	}
}

func hostOf(addr string) string {
	h, _, _ := net.SplitHostPort(addr)
	return h
}
