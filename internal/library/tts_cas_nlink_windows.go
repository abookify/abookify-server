//go:build windows

package library

import "os"

// fileNlink on Windows: os.Stat exposes no link count (Sys() is a
// Win32FileAttributeData), and syscall.Stat_t does not exist there — that
// undefined symbol broke the whole Windows build (desktop-release run
// 35287630260). Report "referenced" (2) so the GC NEVER deletes a CAS blob it
// cannot prove unreferenced: conservative, disk may hold stale blobs on
// Windows until a real count (GetFileInformationByHandle.NumberOfLinks via
// golang.org/x/sys/windows) is wired. Correctness over reclaim.
func fileNlink(info os.FileInfo) uint64 { return 2 }
