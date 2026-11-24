package main

import "golang.org/x/sys/unix"

// restoreTerminalFlags は終了時にターミナルフラグを復元する（LF を CRLF に変換する ONLCR）。
func restoreTerminalFlags(fd int) {
	termios, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return
	}
	termios.Oflag |= unix.ONLCR
	_ = unix.IoctlSetTermios(fd, ioctlSetTermios, termios)
}
