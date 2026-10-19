//go:build linux

package bootstraprunner

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// One process group per job command; never signal the runner service's group.
// The reviewed local runner must not daemonize or create additional sessions.
func configureJobProcess(command *exec.Cmd) (func(), error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killGroup := func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.Cancel = killGroup
	// Also remove descendants retaining output pipes after a direct child exit.
	return func() { _ = killGroup() }, nil
}
