// Package proc ties helper processes (aria2, FFmpeg) to the server's life.
package proc

import (
	"os/exec"
	"syscall"
)

// DieWithParent asks the kernel to send sig to cmd's process if the server dies, even when killed
// outright; without it a helper could outlive the server and race the one started after a restart.
// It has an effect on Linux only, where the server runs.
func DieWithParent(cmd *exec.Cmd, sig syscall.Signal) {
	dieWithParent(cmd, sig)
}
