//go:build !linux

package proc

import (
	"os/exec"
	"syscall"
)

func dieWithParent(*exec.Cmd, syscall.Signal) {}
