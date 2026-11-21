//go:build !unix && !windows

package upstream

import (
	"errors"
	"os"
	"os/exec"
)

// startNewGroup does nothing: this platform has no process groups Wayfindr
// can use.
func startNewGroup(*exec.Cmd) {}

// procGroup is a started stdio child alone. Processes it starts are not
// tracked, and may outlive it.
type procGroup struct {
	proc *os.Process
}

func newProcGroup(p *os.Process) (*procGroup, error) { return &procGroup{proc: p}, nil }

func (g *procGroup) terminate() error { return errors.ErrUnsupported }

func (g *procGroup) kill() error { return g.proc.Kill() }

// alive is false: once the child has been reaped nothing is left to stop.
func (g *procGroup) alive() bool { return false }

func (g *procGroup) release() {}
