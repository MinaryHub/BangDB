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

// lockOffset is the byte locked to claim the database: far beyond any data
// (Windows locks are mandatory, so locking the data itself would stop
// other programs, e.g. backup tools, from reading the file).
const lockOffset = 0x7FFFFFFF_00000000

// lockFile takes an exclusive lock (LockFileEx) on one byte past the data
// so that only one process opens the database at a time.
func lockFile(f *os.File) (func(), error) {
	ol := syscall.Overlapped{Offset: uint32(lockOffset & 0xFFFFFFFF), OffsetHigh: uint32(lockOffset >> 32)}
	h := syscall.Handle(f.Fd())
	r, _, err := procLockFileEx.Call(uintptr(h), lockfileExclusiveLock|lockfileFailImmediately, 0,
		1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		if err == errorLockViolation || err == syscall.ERROR_IO_PENDING {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() {
		ol := syscall.Overlapped{Offset: uint32(lockOffset & 0xFFFFFFFF), OffsetHigh: uint32(lockOffset >> 32)}
		procUnlockFileEx.Call(uintptr(h), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	}, nil
}
