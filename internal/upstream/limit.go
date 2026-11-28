package upstream

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
)

// Limits on what one upstream can make the daemon buffer (design §7.2,
// wayfindr-cq80). None is a setting: a tool that answers with more than
// 16 MiB in one message is misbehaving or not meant for agents.
const (
	// maxMessageBytes bounds one upstream message: an HTTP response body
	// that isn't an event stream, one SSE event, or one stdio line.
	// Options.MaxMessageBytes lowers it in tests.
	maxMessageBytes = 16 << 20
	// maxSyncPages bounds the pages one ListTools follows, and
	// maxSyncTools the tools it keeps: each tool is also embedded and
	// stored, so a list needs a cap on its length as well as on its bytes.
	// Both are about ten times the largest servers in use (a few hundred
	// tools).
	maxSyncPages = 50
	maxSyncTools = 2000
	// maxSyncBytes bounds the tool definitions one ListTools keeps
	// (Options.MaxSyncBytes lowers it in tests), measured
	// as each tool's JSON re-encoded, across every page: without it, 50
	// pages of up to 16 MiB each could add up to some 800 MiB. 32 MiB is
	// 2,000 tools at 16 KiB each, a generous schema; the largest servers
	// in use list a few hundred tools of a few KiB, under 2 MiB in all.
	// A sync then holds at most this much plus the one page it decodes.
	maxSyncBytes = 32 << 20
)

// errMessageTooLarge is what a message over the limit fails to read with.
// The SDK flattens a read error into text, so the Manager learns of the
// overflow from the session's overflow record instead.
var errMessageTooLarge = errors.New("upstream message over the size limit")

// overflow records that one session's transport cut off a message over
// the limit, and how it came (CaseBody or CaseMessage). The SDK reports
// the failure as a closed connection, with the cause flattened into text,
// so the Manager reads the overflow from here into the use's outcome (see
// judge).
type overflow struct {
	mu   sync.Mutex
	kase Case
}

// record notes an overflow of kind c; the first one recorded stays.
func (o *overflow) record(c Case) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.kase == "" {
		o.kase = c
	}
}

// reset forgets an overflow recorded so far. A connect that succeeded
// calls it: an oversized answer to a request of the connect (a failed
// initialize before the SSE fallback, or a refused server/discover) did
// not come over the session that the record now belongs to.
func (o *overflow) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.kase = ""
}

// seen is the Case of the overflow recorded, or "" if none was. A nil
// overflow has seen none.
func (o *overflow) seen() Case {
	if o == nil {
		return ""
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	return o.kase
}

// limitTransport bounds the response bodies the SDK reads whole: every
// one but a successful event stream. The Streamable client reads a JSON
// answer and an error body with io.ReadAll, and an error body whatever its
// Content-Type. Event streams are read one event at a time, each bounded
// by the SDK's MaxEventSize, which dialHTTP sets to the same limit; a cap
// on a stream's total would only kill the long-lived GET.
type limitTransport struct {
	base     http.RoundTripper
	limit    int
	overflow *overflow
}

func (t *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)

	if err != nil {
		return nil, err
	}

	ok := resp.StatusCode >= 200 && resp.StatusCode <= 299

	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ok && mt == "text/event-stream" {
		return resp, nil
	}

	resp.Body = &limitBody{ReadCloser: resp.Body, left: t.limit, overflow: t.overflow}

	return resp, nil
}

// limitBody is a response body that fails with errMessageTooLarge, and
// records the overflow, on its byte limit+1. It reads at most that one
// byte past the limit. Unlike io.LimitReader, it does not end the body
// early as if it were complete, which would read as malformed JSON.
type limitBody struct {
	io.ReadCloser
	left     int // bytes still allowed; -1 once over
	overflow *overflow
}

func (b *limitBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, b.err()
	}

	if len(p) > b.left+1 {
		p = p[:b.left+1]
	}

	n, err := b.ReadCloser.Read(p)
	b.left -= n

	if b.left < 0 {
		b.overflow.record(CaseBody)

		return n - 1, b.err()
	}

	return n, err
}

func (b *limitBody) err() error { return fmt.Errorf("%w: response body", errMessageTooLarge) }

// lineLimitReader is a stdio child's stdout. It fails with
// errMessageTooLarge, and records the overflow, once a line (one message:
// stdio frames messages by newline) passes limit bytes.
type lineLimitReader struct {
	r        io.Reader
	limit    int
	overflow *overflow
	// line counts the bytes read since the last newline; -1 once over.
	line int
}

func (l *lineLimitReader) Read(p []byte) (int, error) {
	if l.line < 0 {
		return 0, l.err()
	}

	n, err := l.r.Read(p)

	for rest := p[:n]; ; {
		i := bytes.IndexByte(rest, '\n')

		if i < 0 {
			l.line += len(rest)

			break
		}

		if l.line+i > l.limit {
			l.line = l.limit + 1

			break
		}

		l.line, rest = 0, rest[i+1:]
	}

	if l.line > l.limit {
		l.line = -1
		l.overflow.record(CaseMessage)

		return 0, l.err()
	}

	return n, err
}

func (l *lineLimitReader) err() error { return fmt.Errorf("%w: stdout line", errMessageTooLarge) }

// syncOverflowError is a tool list longer than ListTools reads: Case is
// CaseSyncPages, CaseSyncTools, or CaseSyncBytes.
type syncOverflowError struct {
	kase   Case
	detail string
}

func (e *syncOverflowError) Error() string {
	return "tool list longer than Wayfindr reads: " + e.detail
}
