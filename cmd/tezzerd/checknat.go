package main

// checknat.go: tezzerd -check-nat — サーバーホストの NAT 環境ワンショット診断。
//
// STUN 経由の直接続に頼る運用では、ユーザーが自身の NAT の性質（マッピングの
// 宛先依存性・port 保存性）を知っていることが重要になる。これは常時表示する
// 情報ではなく「サーバー側で一度知れば良い」ものなので、接続パスには載せず
// 専用コマンドとして提供する（接続パス側の旧 NAT 判別はソケットを分けて
// 問い合わせる誤実装で、ほぼ常に symmetric と誤報告していた経緯がある。
// 正しい同一ソケット判別は stun.Probe 参照）。
//
// 診断はソケット単位ではなく NAT 装置の性質を見るものなので、使い捨て
// ソケットで測って QUIC ポートにも一般化できる。ただし多段 NAT（CGN）は
// 最外層の性質しか見えない。

import (
	"fmt"
	"net"
	"time"

	"github.com/kuriyama/tezzer/internal/stun"
)

const checkNATTimeout = 5 * time.Second

// runCheckNAT は family ごとに stun.Probe を実行して診断を表示する。
// 戻り値は exit code（0 = 少なくとも 1 family で診断成功、1 = 全 family で STUN 到達不可）。
func runCheckNAT(serverA, serverB string, ipv4Only bool) int {
	fmt.Printf("NAT check: querying %s and %s from a single socket per family\n\n", serverA, serverB)

	families := []struct{ network, label string }{{"udp4", "IPv4"}}
	if !ipv4Only {
		families = append(families, struct{ network, label string }{"udp6", "IPv6"})
	}
	diagnosed := false
	for _, f := range families {
		if checkNATFamily(f.network, f.label, serverA, serverB) {
			diagnosed = true
		}
		fmt.Println()
	}

	fmt.Println("Note: results reflect the outermost NAT on the path; stacked NATs (e.g. CGN)")
	fmt.Println("      cannot be distinguished. Mapping behavior is a property of the NAT device,")
	fmt.Println("      so it applies to tezzer's QUIC ports as well.")
	if !diagnosed {
		return 1
	}
	return 0
}

// checkNATFamily は 1 family 分の診断を表示する。診断できたら true。
func checkNATFamily(network, label, serverA, serverB string) bool {
	res, err := stun.Probe(network, serverA, serverB, checkNATTimeout)
	if err != nil {
		fmt.Printf("%s: probe failed: %v\n", label, err)
		return false
	}

	fmt.Printf("%s:\n", label)
	fmt.Printf("  public address:  %s\n", res.MappedA.IP)

	// mapped アドレスが自ホストのインターフェースに直接付いている = NAT なし。
	if isLocalIP(res.MappedA.IP) {
		fmt.Printf("  NAT:             none (public address is assigned to this host)\n")
		fmt.Printf("  verdict:         direct QUIC reachability depends only on firewalls\n")
		return true
	}

	// 宛先依存マッピング（symmetric）の場合、port 保存性は宛先ごとに変わるため
	// 表示しても意味がない（誤解のもとなので出さない）。
	if !res.EndpointIndependent() {
		fmt.Printf("  mapping:         destination-dependent (symmetric: %s saw port %d, %s saw port %d)\n",
			serverA, res.MappedA.Port, serverB, res.MappedB.Port)
		fmt.Printf("  verdict:         STUN-derived candidates will NOT work.\n")
		fmt.Printf("                   Use a fixed port (-udp-port) with router port forwarding,\n")
		fmt.Printf("                   or rely on the SSH-forwarded path.\n")
		return true
	}

	fmt.Printf("  mapping:         endpoint-independent (cone)\n")
	preserving := "no"
	if res.PortPreserving() {
		preserving = "yes"
	}
	fmt.Printf("  port-preserving: %s (local %d -> mapped %d)\n", preserving, res.LocalPort, res.MappedA.Port)

	if res.PortPreserving() {
		fmt.Printf("  verdict:         direct QUIC from outside should work\n")
		fmt.Printf("                   (tezzer advertises public IP + QUIC listen port).\n")
	} else {
		fmt.Printf("  verdict:         cone NAT but not port-preserving: the advertised candidate\n")
		fmt.Printf("                   (public IP + listen port) will not match the actual mapping.\n")
		fmt.Printf("                   Prefer a fixed port (-udp-port) with router port forwarding.\n")
	}
	return true
}

// isLocalIP は ip が自ホストのインターフェースに割り当てられているかを返す。
func isLocalIP(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
			return true
		}
	}
	return false
}
