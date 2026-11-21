//go:build !unix

package board

import "os/exec"

// setProbeGroup is a no-op where process groups are not the mechanism: the default
// single-process kill of exec.CommandContext is all this platform offers.
func setProbeGroup(cmd *exec.Cmd) {}

// killProbeGroup kills the probe itself. Children it started are not reachable on this
// platform, and pretending otherwise would be a comment that lies.
func killProbeGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
