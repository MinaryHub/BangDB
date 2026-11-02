//go:build !unix && !windows

package pager

import "os"

// lockFile is a no-op on platforms without file locks (e.g. plan9, wasm).
// Callers must ensure that only one process opens the database file.
func lockFile(f *os.File) (func(), error) { return func() {}, nil }
