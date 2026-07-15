package main

// agent.go: SSH agent forwarding（-A）のクライアント側 CLI ヘルパー。
// 実際の dial・中継は internal/qtransport/agent.go（quicClient）が担う。ここでは
// ローカル $SSH_AUTH_SOCK の解決と、サーバ非対応時の警告のみを扱う。

import (
	"os"
)

// resolveLocalAgentSockPath はローカルの $SSH_AUTH_SOCK が使えそうかを確認し、
// 使えるならそのパスを、そうでなければ空文字を返す。
// 空文字の間は Hello の AgentForward が false になり、サーバからの agent 中継
// 要求にも応じない（qtransport 側で ctrlAgentOpenErr を返す）。
func resolveLocalAgentSockPath() string {
	path := os.Getenv("SSH_AUTH_SOCK")
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return ""
	}
	return path
}

// warnIfAgentForwardingUnsupported は接続確立後、サーバが agent forwarding 非対応
// （旧サーバ or --no-agent-forwarding）なら一度だけ警告する。forward.go の
// pollForFeatureSupport を共用する。
func (c *Client) warnIfAgentForwardingUnsupported() {
	c.pollForFeatureSupport(func() (bool, bool) {
		af, ok := c.transport().(interface{ AgentForwardingSupported() bool })
		return ok && af.AgentForwardingSupported(), c.transport() != nil
	},
		"-A: server does not support agent forwarding (old server or --no-agent-forwarding)",
		"-A: QUIC transport not established; agent forwarding unavailable")
}
