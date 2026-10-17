package netx

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// GetPeerUID は tezzerd の UDS 制御ソケット認証（cmd/tezzerd/main.go の
// UID 照合、UID 不一致は ErrUnauthorized）が実際に依拠している関数だが、
// テストが一本もなかった。異なる UID からの拒否は権限的にテストしづらいので、
// 「自分自身との UDS 接続で自分の UID が正しく取れること」を実ソケットで検証する
// （SO_PEERCRED/LOCAL_PEERCRED の呼び出し経路そのものの回帰保険）。
func TestGetPeerUID_Self(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	cli, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	var srv net.Conn
	select {
	case srv = <-accepted:
		defer srv.Close()
	case err := <-acceptErr:
		t.Fatalf("Accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Accept")
	}

	uid, err := GetPeerUID(srv)
	if err != nil {
		t.Fatalf("GetPeerUID: %v", err)
	}
	if want := uint32(os.Getuid()); uid != want {
		t.Errorf("GetPeerUID = %d, want %d (own uid, since the test dials itself)", uid, want)
	}
}

// GetPeerUID は net.UnixConn 以外（例えば net.Pipe の両端）を渡されたら
// 明示エラーを返す契約になっている（呼び出し側はこれを認証失敗として扱う）。
func TestGetPeerUID_NotUnixConn(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	if _, err := GetPeerUID(a); err == nil {
		t.Fatal("GetPeerUID on a non-Unix net.Conn succeeded, want error")
	}
}
