package main

// rpc.go: UDS 上の 1 リクエスト 1 レスポンス往復の共通化。
// 管理コマンド（-list/-info/-kill）とセッション確立（CREATE/ATTACH）は全て
// 「Encode → WriteFrame → ReadFrame → Decode → 型アサート（ErrorMsg フォールバック）」
// という同じ骨格なので、ここに集約する。

import (
	"fmt"
	"log"
	"net"

	"github.com/kuriyama/tezzer/internal/netx"
	"github.com/kuriyama/tezzer/internal/proto"
)

// roundTrip は req を書き、T 型の応答を 1 つ読む。サーバが ErrorMsg を返した場合は
// その内容をエラーとして返す。
func roundTrip[T any](conn net.Conn, req any) (*T, error) {
	data, err := proto.Encode(req)
	if err != nil {
		return nil, fmt.Errorf("encode error: %w", err)
	}
	if err := netx.WriteFrame(conn, data); err != nil {
		return nil, fmt.Errorf("write error: %w", err)
	}
	frameData, err := netx.ReadFrame(conn)
	if err != nil {
		return nil, fmt.Errorf("read error: %w", err)
	}
	msg, err := proto.Decode(frameData)
	if err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}
	resp, ok := msg.(*T)
	if !ok {
		if errMsg, ok := msg.(*proto.ErrorMsg); ok {
			return nil, fmt.Errorf("server error: %s: %s", errMsg.Code, errMsg.Message)
		}
		return nil, fmt.Errorf("expected %T, got %T", (*T)(nil), msg)
	}
	return resp, nil
}

// dialAndHandshake はサーバーに接続し HELLO/WELCOME ハンドシェイクを行う
func dialAndHandshake(addr string, cols, rows int) (net.Conn, error) {
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return nil, err
	}

	helloData, err := proto.Encode(proto.HelloMsg{
		Type:       "HELLO",
		V:          1,
		ClientName: "tezzer",
		Cols:       cols,
		Rows:       rows,
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to encode hello: %w", err)
	}
	if err := netx.WriteFrame(conn, helloData); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to write hello: %w", err)
	}

	frameData, err := netx.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read welcome: %w", err)
	}
	msg, err := proto.Decode(frameData)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to decode welcome: %w", err)
	}
	welcome, ok := msg.(*proto.WelcomeMsg)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("expected WELCOME, got %T", msg)
	}
	log.Printf("connected to %s", welcome.ServerName)

	return conn, nil
}

// connectToServer は管理コマンド用のサーバー接続（端末サイズは不問）
func connectToServer(addr string) (net.Conn, error) {
	return dialAndHandshake(addr, 80, 24)
}
