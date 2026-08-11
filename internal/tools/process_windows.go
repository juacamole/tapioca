//go:build windows

package tools

import (
	"os/exec"
	"strconv"
	"syscall"
)

// createNewProcessGroup is CREATE_NEW_PROCESS_GROUP. Spelling it out avoids a
// dependency on golang.org/x/sys for a single constant.
const createNewProcessGroup = 0x00000200

// shellFor picks an interpreter. The agent writes POSIX shell, so a real sh —
// from Git for Windows, MSYS or WSL — is used when one is on PATH; cmd.exe is
// the fallback, and commands written for sh will mostly fail there.
func shellFor(command string) (string, []string) {
	if sh, err := exec.LookPath("sh"); err == nil {
		return sh, []string{"-c", command}
	}
	return "cmd", []string{"/C", command}
}

// isolateProcess starts the command in its own process group and kills the
// whole tree on cancellation. Windows has no process-group signal, so the
// tree is torn down with taskkill; killing only the shell would leave whatever
// it spawned running.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill() // taskkill missing or refused; at least stop the shell
		}
		return nil
	}
}
