//go:build linux

package shadow

import "syscall"

// pryonSysProcAttr puts the helper in its own process group, so it can be
// killed as a unit and a signal to the server's group does not reach it, and
// asks the kernel to SIGKILL it when the server dies (GOOS=android builds
// with this file too).
//
// Pdeathsig is the second line of defence, not the first: it is tied to the
// OS THREAD that forked, and the Go runtime could in principle retire that
// thread while the server lives on (nothing in the device code locks threads,
// so it does not in practice). The first line is stdin EOF, which ends the
// helper whenever the server's end of the pipe closes, and the helper sets
// PR_SET_PDEATHSIG on itself as well. Should a retired thread ever kill a
// helper this way, it counts as a crash like any other early death; five of
// them in a row would fall back to openWakeWord, not leave the device deaf.
func pryonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
