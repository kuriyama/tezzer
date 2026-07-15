//go:build linux

package netx

import (
	"fmt"
	"net"
	"syscall"
)

// GetPeerUID returns the UID of the peer connected to a Unix Domain Socket
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

	// Get peer credentials using SO_PEERCRED
	ucred, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, fmt.Errorf("failed to get peer credentials: %w", err)
	}

	return ucred.Uid, nil
}
