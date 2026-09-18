//go:build !windows

package library

import (
	"os"
	"syscall"
)

// fileNlink reports the hard-link count of a stat'd file. The CAS keeps one
// hard link per edition that references a blob, so nlink<=1 means "no edition
// references this any more" and the GC may remove it after the grace period.
// The second result is false when the count cannot be read; the GC then
// leaves the file alone — an unknown count is never treated as a number.
func fileNlink(_ string, info os.FileInfo) (uint64, bool) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink), true
	}
	return 0, false
}
