//go:build !windows

package diskfree

import "syscall"

// Free returns the bytes available to an unprivileged user on the filesystem
// containing path, or 0 if it can't be determined.
func Free(path string) int64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
