//go:build !linux

package server

import "syscall"

// engineSysProcAttr: Pdeathsig is Linux-only; on macOS/Windows the Tauri shell
// owns engine teardown, and the server's stopInstalledEngine() SIGTERM covers the
// same-session case.
func engineSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
