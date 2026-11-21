//go:build unix

package board

import (
	"os/exec"
	"syscall"
)

// setProbeGroup puts the probe in its own process group, so the whole tree it starts
// can be killed at once. A probe is arbitrary shell: `pipeline | filter &` leaves a
// child holding the pipe, and killing only the shell would leave that child running
// long after the board gave up on the observation.
func setProbeGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProbeGroup kills the probe's process group. It is what runs when the probe's
// deadline expires, in place of the single-process kill exec.CommandContext installs by
// default: a timeout means "this observation is abandoned", and everything it started
// is part of the observation.
//
// The fallback matters: if the group kill fails (no process, or a group that has
// already gone), the process itself is still killed, so a probe can never survive its
// own timeout because of a bookkeeping error.
func killProbeGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
