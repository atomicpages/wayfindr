package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
)

// stopGrace is how long stopping a stdio child waits at each step: for the
// child to exit after its stdin closes, after the terminate signal, and
// after the kill. It is the SDK's CommandTransport default.
const stopGrace = 5 * time.Second

// stdioTransport runs a connector's command and speaks MCP over its stdin
// and stdout. It takes the place of mcp.CommandTransport, which stops only
// the direct child: launchers such as npx and uvx run the real server as a
// grandchild, which outlived it. stdioTransport starts the child in a
// process group of its own (a job object on Windows) and stops the whole
// group.
type stdioTransport struct {
	cmd *exec.Cmd
	log zerolog.Logger
	// limit bounds one line of the child's stdout, and overflow records a
	// line cut off for passing it (see lineLimitReader).
	limit    int
	overflow *overflow
}

// Connect starts the command.
func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	stdout, err := t.cmd.StdoutPipe()

	if err != nil {
		return nil, fmt.Errorf("stdio child stdout pipe: %w", err)
	}

	stdin, err := t.cmd.StdinPipe()

	if err != nil {
		return nil, fmt.Errorf("stdio child stdin pipe: %w", err)
	}

	startNewGroup(t.cmd)

	if err := t.cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %w", errSpawn, err)
	}

	g, err := newProcGroup(t.cmd.Process)

	if err != nil {
		// The child still runs, and stopping it stops it alone.
		t.log.Warn().Err(err).Msg("stdio child not grouped; its own children may outlive it")
	}

	child := &stdioChild{cmd: t.cmd, stdin: stdin, group: g}
	out := &lineLimitReader{r: stdout, limit: t.limit, overflow: t.overflow}
	// Closing the connection closes stdout's reader, which must not stop
	// the child (Wait closes the pipe), and then child, which does.
	//
	// out bounds each line, and its overflow is recorded. The SDK's own
	// bound on one JSON value is set past it, to stop only a value spread
	// over many lines, which out can't see.
	conn, err := (&mcp.IOTransport{Reader: io.NopCloser(out), Writer: child, MaxLineLength: 2 * t.limit}).Connect(ctx)

	if err != nil {
		// Nothing else would stop the child; its exit error adds nothing.
		_ = child.Close()

		return nil, fmt.Errorf("stdio child connection: %w", err)
	}

	return conn, nil
}

// stdioChild is a running stdio child, seen as the writer of its stdin.
// Closing it stops the child's whole process group.
type stdioChild struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	group *procGroup

	closeOnce sync.Once
	closeErr  error
}

func (c *stdioChild) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close stops the child the way the MCP spec asks: it closes the child's
// stdin and waits for it to exit, then signals it to terminate, then kills
// it. The signals go to the child's whole group. Once the child has exited
// and been reaped, whatever it left behind in its group is stopped too:
// asked to terminate, then killed if still there after stopGrace. Close
// returns the child's exit error, as exec.Cmd.Wait does.
func (c *stdioChild) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.stop() })

	return c.closeErr
}

func (c *stdioChild) stop() error {
	// A failed close still leaves the signals to stop the child.
	_ = c.stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- c.cmd.Wait() }()

	ok, err := waitExit(exited)

	if !ok {
		if c.group.terminate() == nil {
			ok, err = waitExit(exited)
		}

		if !ok {
			_ = c.group.kill()

			if ok, err = waitExit(exited); !ok {
				err = errors.New("stdio child did not exit after kill")
			}
		}
	}

	c.group.stopRemaining()
	c.group.release()

	if err != nil {
		return fmt.Errorf("stdio child: %w", err)
	}

	return nil
}

// waitExit waits up to stopGrace for the child's Wait to return. done says
// it did, and err is what it returned.
func waitExit(exited <-chan error) (done bool, err error) {
	t := time.NewTimer(stopGrace)
	defer t.Stop()

	select {
	case err := <-exited:
		return true, err
	case <-t.C:
		return false, nil
	}
}

// stopRemaining stops what is left in g after its leader has exited: it
// asks the rest to terminate, waits up to stopGrace for them to go, and
// kills any that remain.
func (g *procGroup) stopRemaining() {
	if !g.alive() {
		return
	}

	if g.terminate() == nil {
		deadline := time.Now().Add(stopGrace)

		for g.alive() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}

		if !g.alive() {
			return
		}
	}

	_ = g.kill()
}
