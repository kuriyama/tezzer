//go:build linux

package netx

import "syscall"

// peerUID は SO_PEERCRED で接続相手の UID を返す。
func peerUID(fd int) (uint32, error) {
	ucred, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return ucred.Uid, nil
}
