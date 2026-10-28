package main

import (
	"io/fs"
	"path/filepath"
)

// dirSize returns the total size in bytes of all regular files under dir.
// Used to report the agent's real storage usage against its configured
// quota (STORAGE_SIZE), instead of the whole filesystem's usage.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
