//go:build unix

package executor

import (
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// processGroup runs the command in its own process group, so stopping it
// also stops what the shell started: SIGTERM to the group first, SIGKILL to
// the group when it is still there after waitDelay. The returned func must be
// called after the command ends.
func processGroup(cmd *exec.Cmd) func() {
	var (
		mu    sync.Mutex
		timer *time.Timer
	)

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid

		mu.Lock()
		timer = time.AfterFunc(waitDelay, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		mu.Unlock()

		return syscall.Kill(-pid, syscall.SIGTERM)
	}

	return func() {
		mu.Lock()
		defer mu.Unlock()

		if timer != nil {
			timer.Stop()
		}

		// Children left behind by a finished shell go with it.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}
