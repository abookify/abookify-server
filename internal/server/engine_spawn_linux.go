//go:build linux

package server

import "syscall"

// engineSysProcAttr sets Pdeathsig=SIGTERM so a server-spawned engine dies with
// the server (no orphan), and Setpgid so signals reach it cleanly.
func engineSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}
}
