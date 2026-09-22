//go:build windows

package diskfree

import (
	"syscall"
	"unsafe"
)

// Free returns the bytes available to the caller on the volume containing
// path via kernel32!GetDiskFreeSpaceExW (stdlib only — no x/sys dependency).
func Free(path string) int64 {
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
