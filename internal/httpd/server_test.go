package httpd_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/httpd"
)

func TestServerServesAndShutsDown(t *testing.T) {
	ctx := context.Background()
	ln, err := httpd.Listen(ctx, "127.0.0.1:0")

	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	srv, err := httpd.NewServer(ln, httpd.Config{Token: testToken, Version: testVersion, Logger: zerolog.Nop()})

	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	// The Host guard uses the listener's real port, not Config.Port.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr().String()+"/healthz", nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK || string(body) != `{"version":"v1.2.3"}`+"\n" {
		t.Fatalf("GET /healthz = %d %q", resp.StatusCode, body)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx, time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := <-served; err != nil {
		t.Fatalf("Serve after Shutdown = %v, want nil", err)
	}
}

// A daemon on another loopback address answers requests that name it
// (wayfindr-cqet). Only Linux has all of 127.0.0.0/8 on its loopback
// interface by default, so the test skips where 127.0.0.2 can't be bound.
func TestServerAnswersOnAnotherLoopbackAddress(t *testing.T) {
	ctx := context.Background()
	ln, err := httpd.Listen(ctx, "127.0.0.2:0")

	if err != nil {
		t.Skipf("127.0.0.2 can't be bound here: %v", err)
	}

	srv, err := httpd.NewServer(ln, httpd.Config{Token: testToken, Version: testVersion, Logger: zerolog.Nop()})

	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr().String()+"/healthz", nil)

	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz on %s = %d, want 200", srv.Addr(), resp.StatusCode)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx, time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := <-served; err != nil {
		t.Fatalf("Serve after Shutdown = %v, want nil", err)
	}
}

// startServer serves the route table with admin behind /admin/ and returns
// the Server and the result of its Serve.
func startServer(t *testing.T, admin http.Handler) (*httpd.Server, <-chan error) {
	t.Helper()
	ln, err := httpd.Listen(context.Background(), "127.0.0.1:0")

	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	srv, err := httpd.NewServer(ln, httpd.Config{
		Token:    testToken,
		Version:  testVersion,
		Logger:   zerolog.Nop(),
		Handlers: httpd.Handlers{Admin: admin},
	})

	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	return srv, served
}

// getAdmin sends an authorized GET /admin/status in the background. The
// returned channel closes once the response is read or the request fails.
func getAdmin(t *testing.T, srv *httpd.Server) <-chan struct{} {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+srv.Addr().String()+"/admin/status", nil)

	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)
	done := make(chan struct{})
	go func() {
		defer close(done)

		resp, err := http.DefaultClient.Do(req)

		if err != nil {
			return
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	return done
}

// TestShutdownCancelsARequestStillRunningAfterTheDrain: a request that is
// still running when the drain ends has its context cancelled, not before,
// and gets the grace to return.
func TestShutdownCancelsARequestStillRunningAfterTheDrain(t *testing.T) {
	type seen struct {
		at  time.Time
		err error
	}
	started := make(chan struct{})
	cancelled := make(chan seen, 1)
	srv, served := startServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		cancelled <- seen{time.Now(), r.Context().Err()}
	}))
	done := getAdmin(t, srv)
	<-started

	deadline := time.Now().Add(100 * time.Millisecond)
	drain, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	if err := srv.Shutdown(drain, 2*time.Second); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}

	got := <-cancelled

	if got.at.Before(deadline) {
		t.Errorf("request cancelled %v before the drain deadline", deadline.Sub(got.at))
	}

	if !errors.Is(got.err, context.Canceled) {
		t.Errorf("request context error = %v, want context.Canceled", got.err)
	}

	if err := <-served; err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}

	<-done
}

// TestShutdownLetsARequestFinishWithinTheDrain: a request that returns
// during the drain is never cancelled.
func TestShutdownLetsARequestFinishWithinTheDrain(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ctxErr := make(chan error, 1)
	srv, served := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		ctxErr <- r.Context().Err()
		w.WriteHeader(http.StatusNoContent)
	}))
	done := getAdmin(t, srv)
	<-started

	drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	shut := make(chan error, 1)
	go func() { shut <- srv.Shutdown(drain, time.Second) }()

	// Serve returns once Shutdown has closed the listener: the drain has
	// begun, with the request still running.
	if err := <-served; err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}

	close(release)

	if err := <-ctxErr; err != nil {
		t.Errorf("request context error during the drain = %v, want nil", err)
	}

	if err := <-shut; err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}

	<-done
}

// TestShutdownReportsARequestThatIgnoresTheCancel: a request still running
// after the drain and the grace makes Shutdown return an error.
func TestShutdownReportsARequestThatIgnoresTheCancel(t *testing.T) {
	const (
		drainFor = 50 * time.Millisecond
		grace    = 100 * time.Millisecond
	)

	started := make(chan struct{})
	release := make(chan struct{})
	srv, served := startServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	done := getAdmin(t, srv)
	<-started

	begin := time.Now()
	drain, cancel := context.WithTimeout(context.Background(), drainFor)
	defer cancel()

	err := srv.Shutdown(drain, grace)
	elapsed := time.Since(begin)
	close(release)

	if err == nil {
		t.Fatal("Shutdown = nil, want an error for the request still running")
	}

	if elapsed < drainFor+grace {
		t.Errorf("Shutdown returned after %v, want at least the drain and the grace (%v)", elapsed, drainFor+grace)
	}

	if err := <-served; err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}

	<-done
}

// TestCheckLoopback: config validation accepts the name localhost, and a hosts
// file can map it anywhere, so Listen checks the address it actually bound.
func TestCheckLoopback(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		ok   bool
	}{
		{"ipv4 loopback", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777}, true},
		{"other 127/8", &net.TCPAddr{IP: net.ParseIP("127.0.1.1"), Port: 7777}, true},
		{"ipv6 loopback", &net.TCPAddr{IP: net.IPv6loopback, Port: 7777}, true},
		{"lan address", &net.TCPAddr{IP: net.ParseIP("192.168.1.10"), Port: 7777}, false},
		{"public address", &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 7777}, false},
		{"all interfaces v4", &net.TCPAddr{IP: net.IPv4zero, Port: 7777}, false},
		{"all interfaces v6", &net.TCPAddr{IP: net.IPv6unspecified, Port: 7777}, false},
		{"not tcp", &net.UnixAddr{Name: "/tmp/sock", Net: "unix"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := httpd.CheckLoopback(tt.addr)

			if tt.ok && err != nil {
				t.Errorf("CheckLoopback = %v, want nil", err)
			}

			if !tt.ok && !errors.Is(err, httpd.ErrNotLoopback) {
				t.Errorf("CheckLoopback = %v, want ErrNotLoopback", err)
			}
		})
	}
}

func TestListenLocalhostIsLoopback(t *testing.T) {
	ln, err := httpd.Listen(context.Background(), "localhost:0")

	if err != nil {
		t.Fatalf("Listen localhost: %v", err)
	}

	_ = ln.Close()
}

func TestListenAddrInUse(t *testing.T) {
	ctx := context.Background()
	ln, err := httpd.Listen(ctx, "127.0.0.1:0")

	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	defer func() { _ = ln.Close() }()

	_, err = httpd.Listen(ctx, ln.Addr().String())

	if !errors.Is(err, httpd.ErrAddrInUse) {
		t.Fatalf("second Listen error = %v, want ErrAddrInUse", err)
	}
}
