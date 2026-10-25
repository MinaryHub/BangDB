//go:build unix

package pager

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock so that only one process opens
// the database at a time.
func lockFile(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("bangdb: database is already opened by another process")
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
