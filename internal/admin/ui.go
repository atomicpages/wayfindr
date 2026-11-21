package admin

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

// UILogin is the body of POST /admin/ui/login: where to send the browser
// to open a web console session (ADR 0016).
type UILogin struct {
	// Path is the console's login path with a one-time code in its
	// fragment, such as "/ui/login#…". The caller joins it to the daemon's
	// base URL; the fragment keeps the code out of request lines and logs.
	Path string `json:"path"`
	// ExpiresAt is when the code stops working if it is not used.
	ExpiresAt time.Time `json:"expires_at"`
}

// uiLogin serves POST /admin/ui/login. Only the default client token
// reaches it (httpd's admin guard), so only the CLI can open a session.
func (a api) uiLogin(w http.ResponseWriter, _ *http.Request) {
	if a.UILogin == nil {
		http.Error(w, "the web console is not available", http.StatusNotImplemented)

		return
	}

	writeJSON(w, a.UILogin())
}

// UILogin calls POST /admin/ui/login: it returns a one-time login path for
// the web console.
func (c *Client) UILogin(ctx context.Context) (UILogin, error) {
	var l UILogin
	err := c.do(ctx, http.MethodPost, pathUILogin, nil, &l)

	return l, err
}

// NewLocalClient returns a Client that serves every request with h, in
// process: no socket, and no bearer, since h is the admin handler itself
// and httpd's guards are not in the way. The web console uses it, so it
// reads exactly what the CLI reads.
func NewLocalClient(h http.Handler) *Client {
	return NewClient("http://wayfindr.local", "", &http.Client{Transport: handlerTransport{h}})
}

// handlerTransport is an http.RoundTripper that calls a handler.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())

	// A server request always has a body; a client one may not.
	if r.Body == nil {
		r.Body = http.NoBody
	}

	rec := &recorder{header: http.Header{}}
	t.h.ServeHTTP(rec, r)

	if rec.code == 0 {
		rec.code = http.StatusOK
	}

	return &http.Response{
		Status:        strconv.Itoa(rec.code) + " " + http.StatusText(rec.code),
		StatusCode:    rec.code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        rec.header,
		Body:          io.NopCloser(&rec.body),
		ContentLength: int64(rec.body.Len()),
		Request:       r,
	}, nil
}

// recorder is the http.ResponseWriter handlerTransport hands the handler.
type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}

	return r.body.Write(b)
}
