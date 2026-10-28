//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// diskSpace reports free and total bytes of the disk holding path, or of its nearest existing parent.
func diskSpace(path string) (free, total uint64) {
	for {
		var st syscall.Statfs_t
		if syscall.Statfs(path, &st) == nil {
			return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize)
		}
		parent := filepath.Dir(path)
		if _, errStat := os.Stat(path); errStat == nil || parent == path {
			return 0, 0
		}
		path = parent
	}
}
