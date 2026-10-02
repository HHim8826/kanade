package proc

import (
	"os/exec"
	"syscall"
)

func dieWithParent(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = sig
}
