package upstream

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Acquire exposes acquire to tests by connector name, for the tests that
// hold a session open across a reap or watch it end. Kept as a test-only
// seam: the public alternative would export *mcp.ClientSession from the
// Manager, which no production caller needs.
func (m *Manager) Acquire(ctx context.Context, connector string) (*mcp.ClientSession, func(), error) {
	c := m.lookup(connector)

	if c == nil {
		return nil, nil, fmt.Errorf("unknown connector %q", connector)
	}

	defer m.notify(c)
	l, err := m.acquire(ctx, c)

	if err != nil {
		return nil, nil, err
	}

	return l.session, l.release, nil
}

// Reap runs one pass of the idle reaper, which otherwise runs once a
// minute. Kept as a test-only seam: the injected clock (Options.Now) sets
// the idle time, but driving the reaper's ticker as well would need a
// ticker option on the public Options that only tests would set.
func (m *Manager) Reap() { m.reap() }
