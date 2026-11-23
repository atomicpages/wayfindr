package httpclient_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/atomicpages/wayfindr/internal/httpclient"
)

// countingServer starts a server on l (or on 127.0.0.1 when l is nil) and
// returns it with the number of requests it has answered.
func countingServer(t *testing.T, l net.Listener) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	if l != nil {
		_ = srv.Listener.Close()
		srv.Listener = l
	}

	srv.Start()
	t.Cleanup(srv.Close)

	return srv, &hits
}

// TestPublicOnly: a request to a non-public address fails with ErrNotPublic
// before it reaches the server, whether the URL names the address or a
// name that resolves to it.
func TestPublicOnly(t *testing.T) {
	v4, v4Hits := countingServer(t, nil)

	u, err := url.Parse(v4.URL)

	if err != nil {
		t.Fatalf("parse %s: %v", v4.URL, err)
	}

	byName := "http://localhost:" + u.Port() + "/"

	cases := []struct {
		name string
		url  string
		hits *atomic.Int64
	}{
		{"loopback IPv4", v4.URL, v4Hits},
		{"name resolving to loopback", byName, v4Hits},
	}

	var lc net.ListenConfig

	if l, err := lc.Listen(context.Background(), "tcp", "[::1]:0"); err == nil {
		v6, v6Hits := countingServer(t, l)
		cases = append(cases, struct {
			name string
			url  string
			hits *atomic.Int64
		}{"loopback IPv6", v6.URL, v6Hits})
	}

	c := httpclient.PublicOnly()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tt.url, nil)

			if err != nil {
				t.Fatalf("new request: %v", err)
			}

			resp, err := c.Do(req)

			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("GET %s succeeded with %s, want ErrNotPublic", tt.url, resp.Status)
			}

			if !errors.Is(err, httpclient.ErrNotPublic) {
				t.Errorf("GET %s: %v, want an error wrapping ErrNotPublic", tt.url, err)
			}

			if n := tt.hits.Load(); n != 0 {
				t.Errorf("server answered %d requests, want none", n)
			}
		})
	}

	// The same server is reachable with an ordinary client, so the refusal
	// above is PublicOnly's.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, v4.URL, nil)

	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := v4.Client().Do(req)

	if err != nil {
		t.Fatalf("plain GET: %v", err)
	}

	_ = resp.Body.Close()
}

// TestPublicOnlyShape: no proxy (its address would be the one checked), and
// the redirect policy and timeout left to the caller (design §7.11).
func TestPublicOnlyShape(t *testing.T) {
	c := httpclient.PublicOnly()

	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want none", c.Timeout)
	}

	if c.CheckRedirect != nil {
		t.Error("CheckRedirect is set, want the default policy")
	}

	tr, ok := c.Transport.(*http.Transport)

	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}

	if tr.Proxy != nil {
		t.Error("Transport.Proxy is set, want no proxy")
	}

	if tr.DialContext == nil {
		t.Error("Transport.DialContext is nil, want the checking dialer")
	}
}
