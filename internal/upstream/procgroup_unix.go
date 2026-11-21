//go:build unix

package upstream

import (
	"os"
	"os/exec"
	"syscall"
)

// startNewGroup makes cmd's child the leader of a new process group, which
// its own children join unless they leave it on purpose.
func startNewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Setpgid = true
}

// procGroup is a started stdio child's process group, whose ID is the
// child's PID. The kernel doesn't reuse that ID while any member of the
// group is alive, so signalling it after the child is reaped reaches only
// what the child left behind.
type procGroup struct {
	pgid int
}

// newProcGroup returns the group p leads. It never fails on unix.
func newProcGroup(p *os.Process) (*procGroup, error) {
	return &procGroup{pgid: p.Pid}, nil
}

// terminate sends SIGTERM to every process in the group.
func (g *procGroup) terminate() error { return syscall.Kill(-g.pgid, syscall.SIGTERM) }

// kill sends SIGKILL to every process in the group.
func (g *procGroup) kill() error { return syscall.Kill(-g.pgid, syscall.SIGKILL) }

// alive reports whether any process is still in the group. An exited
// member that its new parent hasn't reaped yet still counts.
func (g *procGroup) alive() bool { return syscall.Kill(-g.pgid, 0) == nil }

// release does nothing on unix: a process group holds no resource.
func (g *procGroup) release() {}
