//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput            = 0x0001
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
	enableVirtualTerminalInput      = 0x0200
	enableProcessedOutput           = 0x0001
	enableVirtualTerminalProcessing = 0x0004
)

// rawMode turns off the console's line editing and echo on standard input
// and makes it deliver keys as terminal escape sequences (and the output
// understand them), so that every key is read as it is pressed. It returns
// the function that restores the console; ok is false when standard input
// or output is not a console, or the console is too old for this.
func rawMode() (restore func(), ok bool) {
	in, out := syscall.Handle(os.Stdin.Fd()), syscall.Handle(os.Stdout.Fd())
	var inMode, outMode uint32
	if syscall.GetConsoleMode(in, &inMode) != nil || syscall.GetConsoleMode(out, &outMode) != nil {
		return nil, false
	}
	set := func(h syscall.Handle, mode uint32) bool {
		r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
		return r != 0
	}
	if !set(in, inMode&^(enableLineInput|enableEchoInput|enableProcessedInput)|enableVirtualTerminalInput) {
		return nil, false
	}
	if !set(out, outMode|enableProcessedOutput|enableVirtualTerminalProcessing) {
		set(in, inMode)
		return nil, false
	}
	return func() {
		set(in, inMode)
		set(out, outMode)
	}, true
}

// termWidth returns the number of columns of the console window (80 if
// unknown).
func termWidth() int {
	var info struct {
		size, cursor        [2]int16
		attributes          uint16
		window              [4]int16 // left, top, right, bottom
		maxWidth, maxHeight int16
	}
	r, _, _ := procGetConsoleScreenBufferInfo.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&info)))
	if w := int(info.window[2]-info.window[0]) + 1; r != 0 && w > 0 {
		return w
	}
	return 80
}
