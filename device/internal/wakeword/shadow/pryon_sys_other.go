//go:build !linux

package shadow

import "syscall"

// pryonSysProcAttr: without Pdeathsig, which only Linux has. Hosts other than
// the device run the helper only in tests, where stdin EOF ends it.
func pryonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
