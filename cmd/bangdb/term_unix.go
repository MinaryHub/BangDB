//go:build linux || darwin || freebsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// rawMode turns off the terminal's line editing, echo and signal keys on
// standard input, so that every key is read as it is pressed. It returns
// the function that restores the terminal; ok is false when standard input
// is not a terminal.
func rawMode() (restore func(), ok bool) {
	fd := os.Stdin.Fd()
	var old syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&old))); e != 0 {
		return nil, false
	}
	raw := old
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlSetTermios, uintptr(unsafe.Pointer(&raw))); e != 0 {
		return nil, false
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlSetTermios, uintptr(unsafe.Pointer(&old)))
	}, true
}

// termWidth returns the number of columns of the terminal (80 if unknown).
func termWidth() int {
	var ws struct{ row, col, x, y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.col == 0 {
		return 80
	}
	return int(ws.col)
}
