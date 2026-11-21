package shim

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// closedInputMessage is the error a request the shim sent the client gets
// once the client's stdin is closed: no answer can come.
const closedInputMessage = "the client closed its input"

// stdio holds the client's stdin open, as the SDK sees it, until every
// request read from it has been answered on stdout.
//
// The SDK ends a session as soon as its reader hits EOF: it cancels the
// requests still in flight and refuses to write their responses. A client
// that writes its requests and closes stdin at once (a script piping into
// `wayfindr stdio`) would get no answer at all, and the shim would exit 0.
// So stdio tracks the IDs of the requests it reads and of the responses
// written, and reports EOF only once none is outstanding. Meanwhile a
// request the shim sends the client (elicitation/create) can never be
// answered, so stdio answers it with an error, which the shim turns into
// the usual "could not be asked" result.
//
// It sits between the process's stdin and stdout and the SDK's
// mcp.IOTransport, so the SDK keeps its own framing, batching rules, and
// line limit. Messages are re-framed one per line, as stdio MCP requires.
type stdio struct {
	in  *json.Decoder
	out io.Writer

	// buf is what Read still has to hand the SDK. Only the SDK's single
	// reading goroutine touches it, as it does dec.
	buf []byte
	// eof is set once in has ended.
	eof bool

	mu   sync.Mutex
	cond *sync.Cond
	// pending holds the IDs of the client's requests not yet answered.
	pending map[jsonrpc.ID]struct{}
	// asked holds the IDs of the shim's requests to the client not yet
	// answered.
	asked map[jsonrpc.ID]struct{}
	// done is set when stdout fails or the SDK closes the transport: no
	// more responses will be written, so EOF need not wait for them.
	done bool
}

func newStdio(in io.Reader, out io.Writer) *stdio {
	s := &stdio{
		in:      json.NewDecoder(in),
		out:     out,
		pending: make(map[jsonrpc.ID]struct{}),
		asked:   make(map[jsonrpc.ID]struct{}),
	}
	s.cond = sync.NewCond(&s.mu)

	return s
}

// transport is the SDK transport over s.
func (s *stdio) transport() mcp.Transport {
	return &mcp.IOTransport{Reader: stdioReader{s}, Writer: stdioWriter{s}}
}

// stdioReader and stdioWriter split stdio into the two halves IOTransport
// takes, each with a Close that leaves the process's stdin and stdout open.
type (
	stdioReader struct{ s *stdio }
	stdioWriter struct{ s *stdio }
)

func (r stdioReader) Read(p []byte) (int, error) { return r.s.read(p) }

func (r stdioReader) Close() error {
	r.s.stop()

	return nil
}

func (w stdioWriter) Write(p []byte) (int, error) { return w.s.write(p) }

func (w stdioWriter) Close() error {
	w.s.stop()

	return nil
}

func (s *stdio) read(p []byte) (int, error) {
	for {
		if len(s.buf) > 0 {
			n := copy(p, s.buf)
			s.buf = s.buf[n:]

			return n, nil
		}

		if !s.eof {
			var raw json.RawMessage
			err := s.in.Decode(&raw)

			if errors.Is(err, io.EOF) {
				s.eof = true

				continue
			}

			if err != nil {
				return 0, err
			}

			s.mu.Lock()
			s.track(raw, s.readOne)
			s.mu.Unlock()
			s.buf = append(append(s.buf[:0], raw...), '\n')

			continue
		}

		var err error
		s.buf, err = s.waitAtEOF()

		if err != nil {
			return 0, err
		}
	}
}

// waitAtEOF blocks until the SDK may see EOF, returning it then, or until
// a request to the client needs an error response, returning the response
// to hand the SDK.
func (s *stdio) waitAtEOF() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for {
		if len(s.asked) > 0 {
			var out []byte

			for id := range s.asked {
				msg, err := jsonrpc.EncodeMessage(&jsonrpc.Response{
					ID:    id,
					Error: &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: closedInputMessage},
				})

				if err != nil {
					return nil, err
				}

				out = append(append(out, msg...), '\n')
			}

			clear(s.asked)

			return out, nil
		}

		if len(s.pending) == 0 || s.done {
			return nil, io.EOF
		}

		s.cond.Wait()
	}
}

func (s *stdio) write(p []byte) (int, error) {
	s.mu.Lock()
	s.track(p, s.wroteOne)
	s.cond.Broadcast()
	s.mu.Unlock()

	n, err := s.out.Write(p)

	if err != nil {
		s.stop()
	}

	return n, err
}

// stop releases a reader waiting at EOF: nothing more will be written.
func (s *stdio) stop() {
	s.mu.Lock()
	s.done = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// track calls one for each message in data, a single JSON-RPC message or a
// batch. Data that doesn't decode is the SDK's to refuse, so it is skipped.
// The caller holds s.mu.
func (s *stdio) track(data []byte, one func(jsonrpc.Message)) {
	data = bytes.TrimSpace(data)
	raws := []json.RawMessage{data}

	if bytes.HasPrefix(data, []byte("[")) {
		if json.Unmarshal(data, &raws) != nil {
			return
		}
	}

	for _, raw := range raws {
		if msg, err := jsonrpc.DecodeMessage(raw); err == nil {
			one(msg)
		}
	}
}

// readOne records a message from the client. The caller holds s.mu.
func (s *stdio) readOne(msg jsonrpc.Message) {
	switch m := msg.(type) {
	case *jsonrpc.Request:
		if m.IsCall() {
			s.pending[m.ID] = struct{}{}
		}
	case *jsonrpc.Response:
		delete(s.asked, m.ID)
	}
}

// wroteOne records a message to the client. The caller holds s.mu.
func (s *stdio) wroteOne(msg jsonrpc.Message) {
	switch m := msg.(type) {
	case *jsonrpc.Request:
		if m.IsCall() {
			s.asked[m.ID] = struct{}{}
		}
	case *jsonrpc.Response:
		delete(s.pending, m.ID)
	}
}
