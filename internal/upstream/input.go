package upstream

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// InteractionTTL is how long a parked interaction waits for the user's
// answer (GLOSSARY: Interaction). It matches auth flows, since both wait on
// a person.
const InteractionTTL = 10 * time.Minute

// Input is the user's answers to the input requests a call's result asked
// for, and that result's RequestState.
type Input struct {
	State     string
	Responses mcp.InputResponseMap
}

// The prefixes of the RequestState a Call or Continue result carries. They
// tell Continue how the upstream asked.
const (
	// stateUpstream: the upstream returned input_required (2026-07-28);
	// its own requestState follows.
	stateUpstream = "u:"
	// stateInteraction: the upstream sent elicitation/create during the
	// call (2025-x); the parked interaction's id follows.
	stateInteraction = "i:"
)

// elicitKey is the id of the one input request a parked interaction asks.
const elicitKey = "elicit"

// Continue resumes a call whose result asked for input, with the user's
// answers. tool and args are those of the call that asked. The result is
// Call's: final, or asking again.
//
// An upstream at 2026-07-28 gets a new tools/call carrying the answers and
// its requestState. An upstream at 2025-x gets the answer to its parked
// elicitation/create, and Continue waits for the call's result or its next
// question. An interaction that expired, was discarded, or was answered
// already gives KindInputExpired.
func (m *Manager) Continue(ctx context.Context, name, tool string, args json.RawMessage, in Input) (*mcp.CallToolResult, error) {
	switch {
	case strings.HasPrefix(in.State, stateUpstream):
		return m.call(ctx, name, &mcp.CallToolParams{
			Name:           tool,
			Arguments:      arguments(args),
			RequestState:   strings.TrimPrefix(in.State, stateUpstream),
			InputResponses: in.Responses,
		})
	case strings.HasPrefix(in.State, stateInteraction):
		it := m.take(strings.TrimPrefix(in.State, stateInteraction))

		// An abandoned run's question was answered cancel already (its
		// session closed): what it returns is not the user's answer.
		if it == nil || it.run.c.name != name || it.run.tool != tool || it.run.isAbandoned() {
			if it != nil {
				it.run.abandon()
			}

			return nil, &Error{Connector: name, Kind: KindInputExpired}
		}

		answer, ok := in.Responses[elicitKey].(*mcp.ElicitResult)

		if !ok {
			// No answer to the question: ask it again (SEP-2322).
			return asking(it.ask, m.park(it.run, it.ask)), nil
		}

		it.run.clock.resume()
		it.ask.reply <- answer

		return it.run.wait(ctx, m)
	default:
		return nil, &Error{Connector: name, Kind: KindInputExpired}
	}
}

// Discard gives up on the call whose result asked for input with state,
// because no one will answer: the upstream gets a cancel and finishes on
// its own. A state that parks nothing is ignored.
func (m *Manager) Discard(state string) {
	id, ok := strings.CutPrefix(state, stateInteraction)

	if !ok {
		return
	}

	if it := m.take(id); it != nil {
		it.run.abandon()
	}
}

// run is one tools/call to an upstream, from Call until its final result.
// While the upstream waits on a parked interaction, the run outlives the
// Call that started it: it holds the session lease, and the connector's
// timeout is paused.
type run struct {
	c    *connector
	tool string
	// asks carries the upstream's questions from the elicitation handler
	// to wait.
	asks chan *ask
	// returned is closed once the tools/call has returned; done, once res
	// and err are set.
	returned chan struct{}
	done     chan struct{}
	res      *mcp.CallToolResult
	err      error

	// cancel ends the run's context; the cause is errUseTimeout or the
	// waiting caller's context error.
	cancel context.CancelCauseFunc
	clock  *clock

	// abandoned is closed once no one will answer the run's questions.
	abandoned   chan struct{}
	abandonOnce sync.Once

	// ambiguous is set when the elicitation handler declined a question
	// while r and another call were in flight (see Manager.elicit).
	ambiguous atomic.Bool
	// parked is set while r's question waits for the user, filed as an
	// interaction.
	parked atomic.Bool
}

// addRun records r as one of c's calls in flight.
func (c *connector) addRun(r *run) {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()

	if c.runs == nil {
		c.runs = map[*run]struct{}{}
	}

	c.runs[r] = struct{}{}
}

// removeRun records that r's tools/call has returned.
func (c *connector) removeRun(r *run) {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()
	delete(c.runs, r)
}

// soleRun returns c's only call in flight, or nil. With several, it marks
// each ambiguous, since any of them may be the one that asked.
func (c *connector) soleRun() *run {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()

	if len(c.runs) == 1 {
		for r := range c.runs {
			return r
		}
	}

	for r := range c.runs {
		r.ambiguous.Store(true)
	}

	return nil
}

// errSessionClosed is the cause a parked call's context carries when its
// session is closed under it.
var errSessionClosed = errors.New("upstream session closed")

// parkedCloseGrace is how long a session's close gives an upstream told
// cancel to end its parked call before cancelling it.
const parkedCloseGrace = 5 * time.Second

// endRuns readies c's calls in flight for their session's close, which
// waits for them. Every one is abandoned, so a later question is answered
// cancel. A parked one would hold the close, and c.mu with it, until the
// user answered: its question is answered cancel, and the upstream gets
// parkedCloseGrace to return before the call is cancelled. The answer has
// to arrive first: once the close starts, the SDK sends only
// notifications. A call that is working goes on, as before.
func (c *connector) endRuns() {
	c.runsMu.Lock()
	rs := make([]*run, 0, len(c.runs))

	for r := range c.runs {
		rs = append(rs, r)
	}

	c.runsMu.Unlock()

	var parked []*run

	for _, r := range rs {
		if r.parked.Load() {
			parked = append(parked, r)
		}

		r.abandon()
	}

	grace := time.NewTimer(parkedCloseGrace)
	defer grace.Stop()

	for _, r := range parked {
		select {
		case <-r.returned:
		case <-grace.C:
			r.cancel(errSessionClosed)
		}
	}
}

// ask is one elicitation/create from the upstream, waiting on reply.
type ask struct {
	params *mcp.ElicitParams
	reply  chan *mcp.ElicitResult
}

// abandon declines the run's pending question and every later one, and
// lets the connector's timeout run again.
func (r *run) abandon() {
	r.abandonOnce.Do(func() {
		close(r.abandoned)
		r.clock.resume()
	})
}

// isAbandoned reports whether r was abandoned.
func (r *run) isAbandoned() bool {
	select {
	case <-r.abandoned:
		return true
	default:
		return false
	}
}

// wait waits for r's final result or its next question. ctx's end cancels
// the call, as long as it is waited on; a parked run is not.
func (r *run) wait(ctx context.Context, m *Manager) (*mcp.CallToolResult, error) {
	stop := context.AfterFunc(ctx, func() { r.cancel(context.Cause(ctx)) })

	select {
	case <-r.done:
		stop()

		return r.res, r.err
	case a := <-r.asks:
		if !stop() || !r.clock.pause() {
			// The caller gave up, or the connector's timeout passed, as the
			// question came: the run is cancelled, so no answer is wanted.
			a.reply <- &mcp.ElicitResult{Action: "cancel"}
			<-r.done

			return r.res, r.err
		}

		return asking(a, m.park(r, a)), nil
	}
}

// asking is the result that puts a, parked under id, to the user.
func asking(a *ask, id string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{elicitKey: a.params},
		RequestState:  stateInteraction + id,
	}
}

// interaction is a parked question: an upstream's elicitation/create that
// waits for the user's answer (GLOSSARY: Interaction).
type interaction struct {
	run   *run
	ask   *ask
	timer *time.Timer
}

// park files a under a new random id until Continue takes it, or the
// interaction TTL passes and it is abandoned.
func (m *Manager) park(r *run, a *ask) string {
	id := rand.Text()
	it := &interaction{run: r, ask: a}

	m.interMu.Lock()
	defer m.interMu.Unlock()
	r.parked.Store(true)
	m.interactions[id] = it
	it.timer = time.AfterFunc(m.interactionTTL, func() {
		if it := m.take(id); it != nil {
			it.run.abandon()
		}
	})

	return id
}

// take removes and returns the interaction filed under id, or nil.
func (m *Manager) take(id string) *interaction {
	m.interMu.Lock()
	defer m.interMu.Unlock()
	it := m.interactions[id]

	if it != nil {
		delete(m.interactions, id)
		it.timer.Stop()
		it.run.parked.Store(false)
	}

	return it
}

// endParked drops every parked interaction, for Close, and abandons its
// call, so the upstream is told cancel. It waits for those calls to end
// before the sessions close, since a cancel sent once a close has started
// goes nowhere. Once ctx ends it cancels the calls still running.
func (m *Manager) endParked(ctx context.Context) {
	m.interMu.Lock()
	its := m.interactions
	m.interactions = map[string]*interaction{}
	m.interMu.Unlock()

	for _, it := range its {
		it.timer.Stop()
		it.run.parked.Store(false)
		it.run.abandon()
	}

	for _, it := range its {
		select {
		case <-it.run.done:
		case <-ctx.Done():
			it.run.cancel(context.Cause(ctx))
		}
	}
}

// elicit is c's elicitation handler. An upstream at 2025-x sends
// elicitation/create while a call waits, with nothing that names the call,
// so the question goes to the connector's only call in flight. With more
// than one, it is declined, and every call in flight is marked so the one
// that asked can say why.
func (m *Manager) elicit(c *connector) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		r := c.soleRun()

		if r == nil {
			return nil, &jsonrpc.Error{Code: codeInputDeclined, Message: ambiguousMessage}
		}

		a := &ask{params: req.Params, reply: make(chan *mcp.ElicitResult, 1)}

		select {
		case r.asks <- a:
		case <-r.abandoned:
			return &mcp.ElicitResult{Action: "cancel"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		select {
		case res := <-a.reply:
			return res, nil
		case <-r.abandoned:
			return &mcp.ElicitResult{Action: "cancel"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ambiguousMessage is the message of Wayfindr's decline of an elicitation
// it can't tie to one call.
const ambiguousMessage = "this upstream asked for input while another call to it was running, so Wayfindr can't tell which call asked"

// clock is a connector timeout that can be paused while the user answers.
// It calls fire once the running time adds up to the timeout.
type clock struct {
	mu        sync.Mutex
	remaining time.Duration
	started   time.Time
	// timer is nil while the clock is paused.
	timer   *time.Timer
	stopped bool
	fire    func()
}

func newClock(timeout time.Duration, fire func()) *clock {
	return &clock{remaining: timeout, started: time.Now(), timer: time.AfterFunc(timeout, fire), fire: fire}
}

// pause stops the clock, and reports whether it was running: false when it
// had fired, or was stopped or paused already.
func (k *clock) pause() bool {
	k.mu.Lock()
	defer k.mu.Unlock()

	running := k.timer != nil && k.timer.Stop()

	if running {
		k.remaining -= time.Since(k.started)
	}

	k.timer = nil

	return running
}

// resume starts a paused clock again. A running clock is left alone.
func (k *clock) resume() {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.timer == nil && !k.stopped {
		k.started = time.Now()
		k.timer = time.AfterFunc(max(k.remaining, 0), k.fire)
	}
}

// stop stops the clock for good.
func (k *clock) stop() {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.timer != nil {
		k.timer.Stop()
	}

	k.timer, k.stopped = nil, true
}
