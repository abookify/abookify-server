//go:build !windows

package server

import "syscall"

// fsFreeBytes returns the bytes available to an unprivileged user on the
// filesystem containing path, or 0 if it can't be determined. Unix variant
// (statfs). The Windows variant lives in diskfree_windows.go so the desktop
// bundle cross-compiles for windows/amd64 (which has no syscall.Statfs).
func fsFreeBytes(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
