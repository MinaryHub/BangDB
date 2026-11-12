//go:build !linux && !darwin && !freebsd && !windows

package main

// rawMode is not available on this system: the shell reads plain lines.
func rawMode() (restore func(), ok bool) { return nil, false }

func termWidth() int { return 80 }
