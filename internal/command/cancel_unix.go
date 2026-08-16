//go:build !windows

package command

import (
	"os"
	"os/exec"
	"syscall"
)

func configureCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Let Git and git-annex remove index.lock and other transactional files.
		// Cmd.WaitDelay remains the hard bound for a process group that ignores
		// termination, while signaling the whole group also releases pipes held
		// by ordinary descendants.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
}
