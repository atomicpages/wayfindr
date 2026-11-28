package gateway

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
)

// The approval queue's limits (ADR 0024).
const (
	// pendingTTL is how long a pending approval waits for the owner, from
	// the call that created it. A denied one keeps it.
	pendingTTL = 24 * time.Hour
	// grantedTTL is how long an approval lasts once the owner grants it.
	grantedTTL = time.Hour
	// maxApprovals bounds a client token instance's live approvals, in any
	// state. A call that would add one more is refused, and nothing is
	// evicted.
	maxApprovals = 8
	// approvalIDBytes is the length of a new approval id in random bytes:
	// 4 hex characters. A collision with a live id grows it.
	approvalIDBytes = 2
)

// ApprovalState is where a pending approval stands (GLOSSARY: Pending
// approval).
type ApprovalState string

// Approval states.
const (
	// ApprovalPending: the call waits for the owner's answer.
	ApprovalPending ApprovalState = "pending"
	// ApprovalGranted: the owner approved it; the next matching call runs
	// and spends it.
	ApprovalGranted ApprovalState = "granted"
	// ApprovalDenied: the owner denied it; matching calls are refused until
	// it expires.
	ApprovalDenied ApprovalState = "denied"
)

// Approval is one entry of the approval queue: a gated call of a remote
// client, held in memory with its arguments until the owner answers it or
// it expires (ADR 0024). The arguments are never stored or logged.
type Approval struct {
	// ID is the short id the owner approves or denies it by.
	ID string
	// Client is the client token's name. The entry is bound to the token's
	// instance, so a token created again under the name matches nothing.
	Client string
	// Connector and Tool are the call's, Tool being the upstream's name.
	Connector, Tool string
	// Class is the tool's class as the call path read it.
	Class config.ToolClass
	// Args are the call's arguments as the client sent them; nil for none.
	Args  json.RawMessage
	State ApprovalState
	// Created is when the first gated call made it.
	Created time.Time
	// Expires is when it stops counting: 24 hours after Created while
	// pending, 1 hour after the owner's approval once granted, and once
	// denied 24 hours after Created or 1 hour after the denial, whichever
	// is later.
	Expires time.Time
}

// ErrNoApproval is the error of Approve and Deny for an id the queue
// doesn't hold, or holds expired.
var ErrNoApproval = errors.New("no such approval")

// ErrApprovalChanged is the error of Approve and Deny when the live entry
// id was created at another time than the owner's answer names: the id
// names another call than the one the owner was shown. Ids are not reused
// within a process for 24 hours, but the queue starts empty after a
// restart, so an id in a listing from before it can name a new call.
var ErrApprovalChanged = errors.New("approval is not the call shown")

// approvalKey is what an approval is bound to: the client token instance,
// the tool, and the digest of the arguments.
type approvalKey struct {
	instance, connector, tool, args string
}

// queued is an approval in the queue.
type queued struct {
	Approval

	key approvalKey
	// seq orders entries by creation, whatever the clock says.
	seq uint64
}

// approvalQueue is the gateway's in-memory approval queue, behind one
// mutex. Expiry is checked on access: a lookup, a listing, a bound count.
// There is no sweep. An expired entry stays until its own call looks it up
// (Case expired), or until any call adds an entry, which first drops every
// expired one, so the queue holds at most what was added since.
type approvalQueue struct {
	mu    sync.Mutex
	byID  map[string]*queued
	byKey map[approvalKey]*queued
	seq   uint64
	// ids is where newID draws its random bytes.
	ids io.Reader
	// retired keeps the id of each entry that left the queue (spent,
	// expired, or dropped) until pendingTTL after it left, so newID doesn't
	// hand it to another call while an owner may still read it in an old
	// listing. Pruned when an entry is added.
	retired map[string]time.Time
	// spent keeps each request state a gated call resumed with, by
	// stateKey, until the state's own expiry, so the state is used once
	// (spendState). Pruned on each use.
	spent map[[sha256.Size]byte]time.Time
}

// newApprovalQueue returns an empty queue whose ids draw on ids; nil means
// crypto/rand.
func newApprovalQueue(ids io.Reader) *approvalQueue {
	if ids == nil {
		ids = rand.Reader
	}

	return &approvalQueue{
		byID: map[string]*queued{}, byKey: map[approvalKey]*queued{}, ids: ids,
		retired: map[string]time.Time{}, spent: map[[sha256.Size]byte]time.Time{},
	}
}

// Approvals returns the live approvals, oldest first. Expired ones are
// left out. Each is a copy, its arguments included.
func (g *Gateway) Approvals() []Approval {
	q := g.approvals
	now := g.now()

	q.mu.Lock()
	defer q.mu.Unlock()

	live := make([]*queued, 0, len(q.byID))

	for _, e := range q.byID {
		if now.Before(e.Expires) {
			live = append(live, e)
		}
	}

	slices.SortFunc(live, func(a, b *queued) int { return cmp.Compare(a.seq, b.seq) })

	out := make([]Approval, len(live))

	for i, e := range live {
		out[i] = e.Approval
		out[i].Args = bytes.Clone(e.Args)
	}

	return out
}

// Approve grants the approval id, pending or denied: the next call from
// its client with the same tool and arguments runs, within 1 hour.
// Approving a granted one changes nothing. created is the Created of the
// entry the owner was shown, or zero to approve whatever entry id names.
// ErrNoApproval when the queue holds no live approval id,
// ErrApprovalChanged when it holds one created at another time.
func (g *Gateway) Approve(id string, created time.Time) error {
	q := g.approvals
	now := g.now()

	q.mu.Lock()
	defer q.mu.Unlock()

	e, err := q.answerable(id, created, now)

	if err != nil {
		return err
	}

	if e.State != ApprovalGranted {
		e.State = ApprovalGranted
		e.Expires = now.Add(grantedTTL)
	}

	return nil
}

// Deny denies the approval id, pending or granted, which revokes a grant
// before it is spent. A denied approval refuses matching calls until 24
// hours after it was created, and for at least 1 hour from the denial, so
// denying a grant made late in that window still sticks. Denying a denied
// one changes nothing. created and the errors are as for Approve.
func (g *Gateway) Deny(id string, created time.Time) error {
	q := g.approvals
	now := g.now()

	q.mu.Lock()
	defer q.mu.Unlock()

	e, err := q.answerable(id, created, now)

	if err != nil {
		return err
	}

	if e.State != ApprovalDenied {
		e.State = ApprovalDenied
		e.Expires = e.Created.Add(pendingTTL)

		// A late denial (of a grant made near the end of the pending
		// lifetime) stands for at least grantedTTL.
		if least := now.Add(grantedTTL); e.Expires.Before(least) {
			e.Expires = least
		}
	}

	return nil
}

// answerable is the entry id the owner answers, if it has not expired by
// now and, unless created is zero, was created at created: ErrNoApproval
// or ErrApprovalChanged otherwise. The times are compared as instants, so
// a created that went through RFC 3339 with nanoseconds, in any zone,
// matches. q.mu is held.
func (q *approvalQueue) answerable(id string, created, now time.Time) (*queued, error) {
	e, ok := q.live(id, now)

	if !ok {
		return nil, ErrNoApproval
	}

	if !created.IsZero() && !created.Equal(e.Created) {
		return nil, ErrApprovalChanged
	}

	return e, nil
}

// live is the entry id, if it has not expired by now. An expired one is
// dropped. q.mu is held.
func (q *approvalQueue) live(id string, now time.Time) (*queued, bool) {
	e, ok := q.byID[id]

	if !ok {
		return nil, false
	}

	if !now.Before(e.Expires) {
		q.remove(e, now)

		return nil, false
	}

	return e, true
}

// remove drops e, and keeps its id out of reuse until pendingTTL from now.
// q.mu is held.
func (q *approvalQueue) remove(e *queued, now time.Time) {
	delete(q.byID, e.ID)
	delete(q.byKey, e.key)
	q.retired[e.ID] = now.Add(pendingTTL)
}

// gated reports whether c's approval rule gates connector's tool, of class
// class (ADR 0024). Only a remote client with approval entries is gated.
// Entries that don't parse fail closed: every call from the client is
// gated.
func (g *Gateway) gated(c clienttoken.Client, connector, tool string, class config.ToolClass) bool {
	if !c.Remote || len(c.Approval) == 0 {
		return false
	}

	rule, err := config.ParseApproval(c.Approval)

	if err != nil {
		g.log.Warn().Err(err).Str("client", c.Name).Msg("approval entries do not parse; every call from the client is gated")

		return true
	}

	return rule.Gates(connector, tool, class)
}

// gate decides whether c may make a gated call of connector's tool, of
// class class, with args now (ADR 0024). It returns nil when the owner
// approved exactly this call, which spends the approval. Otherwise it
// returns the refusal, a KindApproval *Error. name is the qualified name
// the client called.
func (g *Gateway) gate(c clienttoken.Client, name, connector, tool string, class config.ToolClass, args json.RawMessage) error {
	q := g.approvals
	key := approvalKey{instance: c.Instance(), connector: connector, tool: tool, args: argsDigest(args)}
	now := g.now()

	q.mu.Lock()
	defer q.mu.Unlock()

	refuse := func(cs Case, id string) error {
		return &Error{Kind: KindApproval, Case: cs, Tool: name, Connector: connector, Approval: id}
	}

	if e, ok := q.byKey[key]; ok {
		if !now.Before(e.Expires) {
			q.remove(e, now)

			return refuse(CaseApprovalExpired, e.ID)
		}

		switch e.State {
		case ApprovalGranted:
			// Spent before the upstream is called, so it covers one attempt
			// even if that fails, and a concurrent identical call can't
			// spend it too.
			q.remove(e, now)

			return nil
		case ApprovalDenied:
			return refuse(CaseApprovalDenied, e.ID)
		case ApprovalPending:
			return refuse(CaseApprovalPending, e.ID)
		}
	}

	if q.count(key.instance, now) >= maxApprovals {
		return refuse(CaseQueueFull, "")
	}

	q.seq++
	e := &queued{
		Approval: Approval{
			ID:        q.newID(now),
			Client:    c.Name,
			Connector: connector,
			Tool:      tool,
			Class:     class,
			Args:      bytes.Clone(args),
			State:     ApprovalPending,
			Created:   now,
			Expires:   now.Add(pendingTTL),
		},
		key: key,
		seq: q.seq,
	}
	q.byID[e.ID] = e
	q.byKey[key] = e

	g.log.Info().Str("client", c.Name).Str("connector", connector).Str("tool", tool).Str("approval_id", e.ID).
		Msg("call waits for the owner's approval")

	return refuse(CaseApprovalPending, e.ID)
}

// count drops every expired entry, then counts instance's. q.mu is held.
func (q *approvalQueue) count(instance string, now time.Time) int {
	n := 0

	for _, e := range q.byID {
		switch {
		case !now.Before(e.Expires):
			q.remove(e, now)
		case e.key.instance == instance:
			n++
		}
	}

	return n
}

// newID is a random hex id that no entry holds and none held within
// pendingTTL: approvalIDBytes long, one byte longer after each collision.
// It first forgets the retired ids whose time is up. q.mu is held.
func (q *approvalQueue) newID(now time.Time) string {
	for id, until := range q.retired {
		if !now.Before(until) {
			delete(q.retired, id)
		}
	}

	for n := approvalIDBytes; ; n++ {
		b := make([]byte, n)
		_, _ = io.ReadFull(q.ids, b) // crypto/rand never fails; a test's reader shouldn't

		id := hex.EncodeToString(b)
		_, held := q.byID[id]
		_, retired := q.retired[id]

		if !held && !retired {
			return id
		}
	}
}

// spendState records that a gated call resumed with state, a request state
// openState accepted that expires at expires, and reports whether this was
// its first use. A state used once is refused after: the call that minted
// it passed the gate once, so its answer reaches the upstream once, however
// often the client sends it back. A question asked again comes with a new
// state, so an elicitation of several rounds still works. The record is in
// memory, so it holds only for states this process sealed, which the call
// path checks by their boot id first.
func (g *Gateway) spendState(state string, expires time.Time) bool {
	q := g.approvals
	key := stateKey(state)
	now := g.now()

	q.mu.Lock()
	defer q.mu.Unlock()

	for k, until := range q.spent {
		if !now.Before(until) {
			delete(q.spent, k)
		}
	}

	if _, ok := q.spent[key]; ok {
		return false
	}

	q.spent[key] = expires

	return true
}

// stateKey is what spendState knows a request state by: the SHA-256 of its
// sealed bytes, not of its text. Base64 decoding skips line breaks and
// ignores a last character's spare bits, so one sealed state has many
// texts; the sealing (AES-GCM) gives it one byte string.
func stateKey(state string) [sha256.Size]byte {
	sealed, _ := base64.RawURLEncoding.DecodeString(state) // openState decoded it already

	return sha256.Sum256(sealed)
}
