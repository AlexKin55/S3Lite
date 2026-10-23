//go:build !linux

package main

// diskUsage is a stub for non-Linux platforms.
func diskUsage(dir string) (int64, int64) {
	return 0, 0
}
