//go:build !unix

package pager

import "os"

// lockFile is a no-op on platforms without flock. Callers must ensure that
// only one process opens the database file.
func lockFile(f *os.File) (func(), error) { return func() {}, nil }
