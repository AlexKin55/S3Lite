//go:build linux

package main

import "syscall"

// diskUsage returns (freeBytes, usedBytes) of the filesystem holding dir.
func diskUsage(dir string) (int64, int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	blockSize := int64(st.Bsize)
	free := int64(st.Bavail) * blockSize
	total := int64(st.Blocks) * blockSize
	return free, total - free
}
