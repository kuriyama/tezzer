//go:build darwin || freebsd

package main

import "golang.org/x/sys/unix"

// termios の取得・設定の ioctl（macOS / FreeBSD は TIOCGETA/TIOCSETA）
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
