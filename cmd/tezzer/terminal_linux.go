//go:build linux

package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// restoreTerminalFlags は終了時にターミナルフラグを復元します
// Linuxでは TCGETS/TCSETS を使用
func restoreTerminalFlags(fd int) {
	var termios unix.Termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&termios))); err == 0 {
		termios.Oflag |= unix.ONLCR // LFをCRLFに変換
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&termios)))
	}
}
