package main

// rforward.go: -R（リモートポートフォワード）のクライアント側。
// 設計は docs/dev/port-forwarding.md の「Phase 2 設計: `-R`」。
//
// サーバ側の listener は tezzerd が持ち、同じ端末（device ID）からの登録を束ねる。
// クライアントは Hello で listen ポートと device ID を送り、着信ごとに届く中継要求に
// 対して自分の定義の dial 先へ dial する（中継は qtransport 内で完結する）。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kuriyama/tezzer/internal/transport"
)

// maxRemoteForwards は 1 クライアントで指定できる -R の数（サーバ側の上限と揃える）。
const maxRemoteForwards = 16

// remoteForwardSpec は解析済みの -R 指定。
type remoteForwardSpec struct {
	port   uint16 // サーバ側 listen ポート（loopback 固定）
	target string // クライアント側から dial する "host:port"
}

func (r remoteForwardSpec) String() string {
	return fmt.Sprintf("server localhost:%d -> %s", r.port, r.target)
}

// parseRemoteForwardSpec は ssh -R 互換の [bind_address:]port:host:hostport を解釈する。
// サーバ側の bind は loopback 固定なので、bind_address は loopback の表記だけを受け付ける。
func parseRemoteForwardSpec(spec string) (remoteForwardSpec, error) {
	parts := splitForwardParts(spec)
	switch len(parts) {
	case 3:
	case 4:
		bind := stripBrackets(parts[0])
		if bind != "localhost" {
			ip := net.ParseIP(bind)
			if ip == nil || !ip.IsLoopback() {
				return remoteForwardSpec{}, fmt.Errorf("invalid -R bind address %q: only loopback is allowed", bind)
			}
		}
		parts = parts[1:]
	default:
		return remoteForwardSpec{}, fmt.Errorf("invalid -R spec %q (want [bind:]port:host:hostport)", spec)
	}
	port, host, hostport := parts[0], stripBrackets(parts[1]), parts[2]
	if !validPort(port) || !validPort(hostport) {
		return remoteForwardSpec{}, fmt.Errorf("invalid port in -R spec %q (port 0 is not supported)", spec)
	}
	if host == "" {
		return remoteForwardSpec{}, fmt.Errorf("empty host in -R spec %q", spec)
	}
	n, _ := strconv.Atoi(port)
	return remoteForwardSpec{port: uint16(n), target: net.JoinHostPort(host, hostport)}, nil
}

// parseRemoteForwardSpecs は -R 指定群を解釈する（同じポートの重複と上限を検査する）。
func parseRemoteForwardSpecs(specs []string) ([]remoteForwardSpec, error) {
	if len(specs) > maxRemoteForwards {
		return nil, fmt.Errorf("too many -R (max %d)", maxRemoteForwards)
	}
	seen := make(map[uint16]bool)
	var out []remoteForwardSpec
	for _, spec := range specs {
		sp, err := parseRemoteForwardSpec(spec)
		if err != nil {
			return nil, err
		}
		if seen[sp.port] {
			return nil, fmt.Errorf("duplicate -R port %d", sp.port)
		}
		seen[sp.port] = true
		out = append(out, sp)
	}
	return out, nil
}

// deviceID はサーバへ送る端末 ID を返す。~/.tezzer/device-id の乱数（secret）に
// ホスト名を混ぜてハッシュする。ホームディレクトリを複数のマシンで共有・複製している
// 場合（NFS ホーム、移動プロファイル、dotfiles 同期、別マシンへのリストア）に、
// 別々のマシンが同じ ID になって「同じ端末」として束ねられ、着信が別マシンへ
// 届くのを防ぐため。ホスト名そのものは送らない（secret なしでは逆算できない）。
// ホスト名まで同じ複製（VM の丸ごと複製など）は区別できない。
func deviceID(secret, hostname string) string {
	sum := sha256.Sum256([]byte("tezzer-device-id\x00" + secret + "\x00" + hostname))
	return hex.EncodeToString(sum[:16])
}

// deviceIDPath は端末 ID の元になる乱数の保存先（~/.tezzer/device-id）を返す。
func deviceIDPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tezzer", "device-id"), nil
}

// loadOrCreateDeviceID は端末 ID の元になる乱数を読み、無ければ生成して保存する
// （サーバへ送るのはこれにホスト名を混ぜた deviceID）。
// 端末 ID は同じ端末からの -R 登録をサーバ側で束ねるための識別子（認証ではない）。
// 保存できない場合も、このプロセス限りの ID と警告を返して続行する
// （その場合、同じ端末の他のクライアントとは束ねられず「別端末」扱いになる）。
func loadOrCreateDeviceID(path string) (id string, warn error) {
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); validDeviceID(s) {
			return s, nil
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("loadOrCreateDeviceID: " + err.Error())
	}
	id = hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return id, err
	}
	// 複数クライアントの同時初回起動で食い違わないよう、一時ファイル + rename ではなく
	// O_EXCL で作る。負けた側は勝った側の内容を読み直す。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if b, rerr := os.ReadFile(path); rerr == nil {
			if s := strings.TrimSpace(string(b)); validDeviceID(s) {
				return s, nil
			}
		}
		return id, err
	}
	_, werr := f.WriteString(id + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return id, werr
}

func validDeviceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// remoteForwardTargets は qtransport に渡すポート → dial 先の表を返す。
func remoteForwardTargets(specs []remoteForwardSpec) map[uint16]string {
	m := make(map[uint16]string, len(specs))
	for _, sp := range specs {
		m[sp.port] = sp.target
	}
	return m
}

// warnIfRemoteForwardingUnsupported は接続確立後、サーバが -R 非対応
// （旧サーバ or --no-tcp-forwarding）なら一度だけ警告する。
func (c *Client) warnIfRemoteForwardingUnsupported() {
	c.pollForFeatureSupport(func() (bool, bool) {
		rf, ok := c.transport().(transport.RemoteForwardClient)
		return ok && rf.RemoteForwardingSupported(), c.transport() != nil
	},
		"-R: server does not support remote forwarding (old server or --no-tcp-forwarding)",
		"-R: QUIC transport not established; remote forwarding unavailable")
}
