//go:build windows

package library

import (
	"os"
	"syscall"
)

// fileNlink reports the hard-link count of a file on Windows. os.Stat exposes
// no link count there (Sys() is a Win32FileAttributeData, and syscall.Stat_t
// does not exist — that undefined symbol broke the whole Windows build,
// desktop-release run 35287630260). NTFS does keep the count: open the file
// and ask GetFileInformationByHandle for NumberOfLinks — the same figure
// `fsutil hardlink list` walks. The second result is false when the file
// cannot be opened or queried; the GC then leaves it alone rather than guess.
func fileNlink(path string, _ os.FileInfo) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return 0, false
	}
	return uint64(d.NumberOfLinks), true
}
