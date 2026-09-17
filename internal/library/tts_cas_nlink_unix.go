//go:build !windows

package library

import (
	"os"
	"syscall"
)

// fileNlink reports the hard-link count of a stat'd file. The CAS keeps one
// hard link per edition that references a blob, so nlink<=1 means "no edition
// references this any more" and the GC may remove it after the grace period.
func fileNlink(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
