//go:build darwin || freebsd

package netx

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// GetPeerUID returns the UID of the peer connected to a Unix Domain Socket
// on macOS / FreeBSD
func GetPeerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a Unix connection")
	}

	file, err := unixConn.File()
	if err != nil {
		return 0, fmt.Errorf("failed to get file descriptor: %w", err)
	}
	defer file.Close()

	fd := int(file.Fd())

	// macOS / FreeBSD では LOCAL_PEERCRED を使用（どちらも xucred を返す）
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, fmt.Errorf("failed to get peer credentials: %w", err)
	}

	return cred.Uid, nil
}
