// Command conformancekit is the test tooling behind `make conformance`
// (scripts/conformance.sh). It is never part of the wayfindr binary.
//
//	conformancekit freeport
//	    print a free loopback TCP port
//	conformancekit upstream [-version V]
//	    serve a fakeupstream over stdio, for a stdio connector
//	conformancekit proxy -listen ADDR -target HOST:PORT
//	    reverse-proxy ADDR to the daemon, adding the client token as a bearer
//
// The proxy exists because the conformance suite has no way to send an
// Authorization header, while the daemon refuses /mcp without one (design
// §7.7). It reads the token from WAYFINDR_CLIENT_TOKEN and leaves every
// other header alone, so the daemon's Host and Origin guards still see what
// the suite sent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeupstream"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()

	if err != nil {
		fmt.Fprintf(os.Stderr, "conformancekit: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: conformancekit freeport | upstream [-version V] | proxy -listen ADDR -target HOST:PORT")
	}

	switch args[0] {
	case "freeport":
		return freePort(ctx, stdout)
	case "upstream":
		fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
		version := fs.String("version", fakeupstream.V20250618, "protocol version the fake is pinned to")

		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		return fakeupstream.Serve(ctx, *version, &mcp.StdioTransport{})
	case "proxy":
		fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
		listen := fs.String("listen", "", "loopback host:port to listen on")
		target := fs.String("target", "", "daemon host:port")

		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		return serveProxy(ctx, *listen, *target, os.Getenv(clienttoken.EnvVar))
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func freePort(ctx context.Context, stdout io.Writer) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")

	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port

	if err := ln.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	_, err = fmt.Fprintln(stdout, port)

	return err
}

func serveProxy(ctx context.Context, listen, target, token string) error {
	if token == "" {
		return fmt.Errorf("%s is not set", clienttoken.EnvVar)
	}

	h, err := newProxy(listen, target, token)

	if err != nil {
		return err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", listen)

	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// newProxy returns a handler that forwards every request to target with
// "Authorization: Bearer <token>". A Host on the proxy's own port is moved
// to the target's port but keeps its name, so the daemon's Host guard
// judges the name the suite sent (a DNS-rebinding probe's foreign name is
// still refused); a Host on any other port is forwarded unchanged.
func newProxy(listen, target, token string) (http.Handler, error) {
	_, listenPort, err := net.SplitHostPort(listen)

	if err != nil {
		return nil, fmt.Errorf("-listen %q: %w", listen, err)
	}

	_, targetPort, err := net.SplitHostPort(target)

	if err != nil {
		return nil, fmt.Errorf("-target %q: %w", target, err)
	}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = target

			if host, port, err := net.SplitHostPort(pr.In.Host); err == nil && port == listenPort {
				pr.Out.Host = net.JoinHostPort(host, targetPort)
			}

			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		// Stream SSE responses as they arrive.
		FlushInterval: -1,
	}, nil
}
