//go:build windows

package pager

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x2
	lockfileFailImmediately = 0x1
	errorLockViolation      = syscall.Errno(33)
)

// lockFile takes an exclusive lock on the whole file (LockFileEx) so that
// only one process opens the database at a time. Unlike flock, the lock is
// mandatory on Windows: it also stops other processes from writing the
// locked range, which only this process does anyway.
func lockFile(f *os.File) (func(), error) {
	var ol syscall.Overlapped
	h := syscall.Handle(f.Fd())
	r, _, err := procLockFileEx.Call(uintptr(h), lockfileExclusiveLock|lockfileFailImmediately, 0,
		0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		if err == errorLockViolation || err == syscall.ERROR_IO_PENDING {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() {
		var ol syscall.Overlapped
		procUnlockFileEx.Call(uintptr(h), 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)))
	}, nil
}
