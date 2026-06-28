//go:build windows

package server

import (
	"syscall"
	"unsafe"
)

// fsFreeBytes returns the bytes available to the caller on the volume
// containing path, or 0 if it can't be determined. Windows variant via
// kernel32!GetDiskFreeSpaceExW (stdlib only — no x/sys dependency), so the
// desktop bundle cross-compiles for windows/amd64. Mirrors the Unix statfs
// path in diskfree_unix.go.
func fsFreeBytes(path string) int64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	var freeAvailToCaller uint64
	r1, _, _ := proc.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvailToCaller)),
		0, // lpTotalNumberOfBytes (unused)
		0, // lpTotalNumberOfFreeBytes (unused)
	)
	if r1 == 0 { // BOOL FALSE → call failed
		return 0
	}
	return int64(freeAvailToCaller)
}
