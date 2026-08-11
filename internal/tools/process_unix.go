//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// shellFor returns the interpreter for a command string.
func shellFor(command string) (string, []string) {
	return "sh", []string{"-c", command}
}

// isolateProcess puts the command in its own process group and kills the whole
// group on cancellation. Descendants holding the output pipe would otherwise
// keep the read blocked long after the command itself was told to stop.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
