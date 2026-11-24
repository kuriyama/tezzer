//go:build darwin || freebsd

package netx

import "golang.org/x/sys/unix"

// peerUID は LOCAL_PEERCRED（macOS / FreeBSD とも xucred を返す）で接続相手の UID を返す。
func peerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
