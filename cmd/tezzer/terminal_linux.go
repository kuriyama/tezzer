//go:build linux

package main

import "golang.org/x/sys/unix"

// termios の取得・設定の ioctl（Linux は TCGETS/TCSETS）
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
