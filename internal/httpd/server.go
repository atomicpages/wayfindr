package httpd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/rs/zerolog"
)

// readHeaderTimeout is the only timeout on the server (design §7.7). A write
// or idle timeout would cut the long-lived /mcp streams clients hold open.
const readHeaderTimeout = 10 * time.Second

// ErrAddrInUse means the listen address is taken, most likely by a daemon
// that is already running.
var ErrAddrInUse = errors.New("listen address in use")

// ErrNotLoopback means the listener is bound to an address outside
// 127.0.0.0/8 and ::1.
var ErrNotLoopback = errors.New("listen address is not loopback")

// Listen binds addr (config server.listen) and checks that the bound address
// is loopback. Config validation accepts the name localhost, and a hosts file
// can map that name anywhere, so the check is on what was actually bound.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)

	if err != nil {
		if errors.Is(err, errAddrInUse) {
			return nil, fmt.Errorf("listen %s: %w: %w", addr, ErrAddrInUse, err)
		}

		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}

	if err := CheckLoopback(ln.Addr()); err != nil {
		_ = ln.Close()

		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}

	return ln, nil
}

// CheckLoopback returns ErrNotLoopback unless addr is a TCP address in
// 127.0.0.0/8 or ::1.
func CheckLoopback(addr net.Addr) error {
	tcp, ok := addr.(*net.TCPAddr)

	if !ok || !tcp.IP.IsLoopback() {
		return fmt.Errorf("bound to %s: %w", addr, ErrNotLoopback)
	}

	return nil
}

// Server serves the guarded route table on a listener. The daemon owns its
// lifecycle (design §7.8): Listen, NewServer, Serve in a goroutine, and
// Shutdown on SIGINT or SIGTERM.
type Server struct {
	ln  net.Listener
	srv *http.Server
	// cancel cancels the base context every request's context derives
	// from: Shutdown's hard stop.
	cancel context.CancelFunc
	log    zerolog.Logger
}

// NewServer builds a Server on ln. It sets cfg.Port and cfg.IP from ln's
// address, so the Host guard matches the port actually bound (":0"
// included) and accepts the bound IP literal.
func NewServer(ln net.Listener, cfg Config) (*Server, error) {
	tcp, ok := ln.Addr().(*net.TCPAddr)

	if !ok {
		return nil, fmt.Errorf("httpd: listener address %s is not TCP", ln.Addr())
	}

	cfg.Port = tcp.Port
	cfg.IP, _ = netip.AddrFromSlice(tcp.IP)
	h, err := NewHandler(cfg)

	if err != nil {
		return nil, err
	}

	base, cancel := context.WithCancel(context.Background())

	return &Server{
		ln: ln,
		srv: &http.Server{
			Handler:           h,
			ReadHeaderTimeout: readHeaderTimeout,
			BaseContext:       func(net.Listener) context.Context { return base },
		},
		cancel: cancel,
		log:    cfg.Logger,
	}, nil
}

// Addr is the bound listen address.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve accepts connections until Shutdown. It returns nil after Shutdown
// and the accept error otherwise.
func (s *Server) Serve() error {
	if err := s.srv.Serve(s.ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// Shutdown stops accepting connections and drains in-flight requests until
// ctx ends (the daemon gives it 10s). If requests are still running then,
// such as long-lived streams, it cancels every request's context and waits
// up to grace more for them to return. All request contexts are cancelled
// when it returns. It returns an error only if requests are still running
// after the grace.
func (s *Server) Shutdown(ctx context.Context, grace time.Duration) error {
	defer s.cancel()

	err := s.srv.Shutdown(ctx)

	if err == nil {
		return nil
	}

	s.log.Warn().Err(err).Msg("drain timed out; cancelling remaining requests")
	s.cancel()
	// The grace starts now, whatever ended ctx.
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()

	if err := s.srv.Shutdown(stop); err != nil {
		return fmt.Errorf("requests still running after cancel: %w", err)
	}

	return nil
}
