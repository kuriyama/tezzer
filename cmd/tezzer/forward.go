package main

// forward.go: -L（ローカルポートフォワード）のクライアント側。
// 設計は docs/dev/port-forwarding.md。
//
// listener はクライアントローカルに常駐するため、QUIC の reconnect を跨いでも
// トンネル定義は生き残る（進行中の TCP フローは reconnect では切れる。
// migration では QUIC ストリームごと生き残る）。

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/kuriyama/tezzer/internal/transport"
)

// forwardSpec は解析済みの -L 指定。
type forwardSpec struct {
	listenAddr string // クライアント側 listen（loopback のみ）
	target     string // サーバ側から dial する "host:port"
}

func (f forwardSpec) String() string { return f.listenAddr + " -> " + f.target }

// stringListFlag は繰り返し指定できる文字列フラグ（-L 用）。
type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ", ") }
func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

var _ flag.Value = (*stringListFlag)(nil)

// splitForwardParts は ':' で分割する。IPv6 リテラルの [..] 内の ':' は区切りにしない。
func splitForwardParts(s string) []string {
	var parts []string
	var cur strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			depth++
		case r == ']':
			depth--
		case r == ':' && depth == 0:
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	parts = append(parts, cur.String())
	return parts
}

func stripBrackets(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// parseForwardSpec は ssh -L 互換の [bind_address:]port:host:hostport を解釈する。
// bind_address は loopback のみ許可する（設計方針: GatewayPorts 相当は作らない）。
func parseForwardSpec(spec string) (forwardSpec, error) {
	parts := splitForwardParts(spec)
	bind := "127.0.0.1"
	switch len(parts) {
	case 3:
		// port:host:hostport
	case 4:
		bind = stripBrackets(parts[0])
		parts = parts[1:]
	default:
		return forwardSpec{}, fmt.Errorf("invalid -L spec %q (want [bind:]port:host:hostport)", spec)
	}
	port, host, hostport := parts[0], stripBrackets(parts[1]), parts[2]

	if bind != "localhost" {
		ip := net.ParseIP(bind)
		if ip == nil || !ip.IsLoopback() {
			return forwardSpec{}, fmt.Errorf("invalid -L bind address %q: only loopback is allowed", bind)
		}
	}
	if !validPort(port) || !validPort(hostport) {
		return forwardSpec{}, fmt.Errorf("invalid port in -L spec %q", spec)
	}
	if host == "" {
		return forwardSpec{}, fmt.Errorf("empty host in -L spec %q", spec)
	}
	return forwardSpec{
		listenAddr: net.JoinHostPort(bind, port),
		target:     net.JoinHostPort(host, hostport),
	}, nil
}

// startForwardListeners は -L の listener 群を起動する。
// bind 失敗は ssh 同様に警告して継続する（他の forward とセッションは生かす）。
func (c *Client) startForwardListeners() {
	for _, sp := range c.forwards {
		ln, err := net.Listen("tcp", sp.listenAddr)
		if err != nil {
			c.forwardWarn(fmt.Sprintf("-L %s: %v", sp, err))
			continue
		}
		if c.fileLog != nil {
			c.fileLog.Printf("forward: listening %s", sp)
		}
		go func(ln net.Listener) {
			<-c.done
			_ = ln.Close()
		}(ln)
		go c.acceptForwardLoop(sp, ln)
	}
}

func (c *Client) acceptForwardLoop(sp forwardSpec, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener close（done 経由）で終了
		}
		go c.handleForwardConn(sp, conn.(*net.TCPConn))
	}
}

// handleForwardConn は accept した TCP 接続 1 本を QUIC 転送ストリームへ中継する。
func (c *Client) handleForwardConn(sp forwardSpec, tconn *net.TCPConn) {
	fw, _ := c.transport().(transport.TCPForwarder)
	if fw == nil {
		c.forwardWarn(fmt.Sprintf("forward %s: QUIC transport not ready", sp))
		_ = tconn.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	fs, err := fw.OpenForward(ctx, sp.target)
	cancel()
	if err != nil {
		c.forwardWarn(fmt.Sprintf("forward %s: %v", sp, err))
		_ = tconn.Close()
		return
	}

	c.fwdActive.Add(1)
	defer c.fwdActive.Add(-1)

	// 双方向パイプ。半クローズは TCP FIN ↔ ストリーム FIN に対応させる
	// （サーバ側 qtransport.forwardPipe と同じ規約）。
	done := make(chan struct{}, 2)
	go func() {
		if _, err := io.Copy(fs, tconn); err == nil {
			_ = fs.CloseWrite()
		} else {
			_ = fs.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		if _, err := io.Copy(tconn, fs); err == nil {
			_ = tconn.CloseWrite()
		} else {
			_ = tconn.Close()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = tconn.Close()
	_ = fs.Close()
}

// forwardWarn は転送関連の警告を出す。ファイルログには常時、ステータス行は
// 連続失敗のスパム防止のため 5 秒に 1 回に間引く。
func (c *Client) forwardWarn(msg string) {
	if c.fileLog != nil {
		c.fileLog.Printf("%s", msg)
	}
	c.fwdWarnMu.Lock()
	throttled := time.Since(c.lastFwdWarn) < 5*time.Second
	if !throttled {
		c.lastFwdWarn = time.Now()
	}
	c.fwdWarnMu.Unlock()
	if !throttled {
		c.setStatusMessage(msg)
	}
}

// pollForFeatureSupport は QUIC 機能（-L/-A 等）の feature bit 到着（serverMeta 受信）を
// ポーリングし、非対応（または transport 未確立）なら一度だけ警告する。
// check は「(対応しているか, transport が確立済みか)」を返す関数。
func (c *Client) pollForFeatureSupport(check func() (supported, connected bool), unsupportedMsg, notConnectedMsg string) {
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-deadline:
			switch supported, connected := check(); {
			case connected && !supported:
				c.forwardWarn(unsupportedMsg)
			case !connected:
				c.forwardWarn(notConnectedMsg)
			}
			return
		case <-tick.C:
			if supported, connected := check(); connected && supported {
				return // 対応確認できたので警告不要
			}
		}
	}
}

// warnIfForwardingUnsupported は接続確立後、サーバが転送非対応
// （旧サーバ or --no-tcp-forwarding）なら一度だけ警告する。
func (c *Client) warnIfForwardingUnsupported() {
	c.pollForFeatureSupport(func() (bool, bool) {
		fw, ok := c.transport().(transport.TCPForwarder)
		return ok && fw.ForwardingSupported(), c.transport() != nil
	},
		"-L: server does not support TCP forwarding (old server or --no-tcp-forwarding)",
		"-L: QUIC transport not established; forwarding unavailable")
}
