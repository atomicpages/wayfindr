// Command conformancekit is the test tooling behind `make conformance`
// (scripts/conformance.sh). It is never part of the wayfindr binary.
//
//	conformancekit freeport
//	    print a free loopback TCP port
//	conformancekit upstream [-version V]
//	    serve a fakeupstream over stdio, for a stdio connector
//	conformancekit proxy -listen ADDR -target HOST:PORT [-tls]
//	    reverse-proxy ADDR to the daemon, adding the client token as a bearer;
//	    -tls forwards over https with Host set to HOST[:PORT] (a remote
//	    daemon; the default port 443 is dropped from Host, as a browser would)
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
	"strings"
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
		useTLS := fs.Bool("tls", false, "forward over https, Host set to -target")

		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		return serveProxy(ctx, *listen, *target, os.Getenv(clienttoken.EnvVar), *useTLS)
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

func serveProxy(ctx context.Context, listen, target, token string, useTLS bool) error {
	if token == "" {
		return fmt.Errorf("%s is not set", clienttoken.EnvVar)
	}

	h, err := newTargetProxy(listen, target, token, useTLS)

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

// newTargetProxy returns a handler that forwards every request to target with
// "Authorization: Bearer <token>". A Host on the proxy's own port is moved
// to the target's port but keeps its name, so the daemon's Host guard
// judges the name the suite sent (a DNS-rebinding probe's foreign name is
// still refused); a Host on any other port is forwarded unchanged. Either
// way the target's port is left off the Host when it is the scheme's default
// (80).
//
// With useTLS the daemon is reached over https, such as one behind a tunnel.
// Then the forwarded Host is target itself, without the scheme's default port
// (443): the proxy's own name and port mean nothing to that daemon, and a
// tunnel routes on the authority the user wrote.
func newTargetProxy(listen, target, token string, useTLS bool) (http.Handler, error) {
	_, listenPort, err := net.SplitHostPort(listen)

	if err != nil {
		return nil, fmt.Errorf("-listen %q: %w", listen, err)
	}

	_, targetPort, err := net.SplitHostPort(target)

	if err != nil {
		return nil, fmt.Errorf("-target %q: %w", target, err)
	}

	defaultPort := "80"

	if useTLS {
		defaultPort = "443"
	}

	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"

			if useTLS {
				pr.Out.URL.Scheme = "https"
				pr.Out.Host = authority(target, defaultPort)
			} else if host, port, err := net.SplitHostPort(pr.In.Host); err == nil && port == listenPort {
				pr.Out.Host = authority(net.JoinHostPort(host, targetPort), defaultPort)
			}

			// URL.Host keeps the port: it picks the dial address, and the
			// TLS SNI is its bare hostname.
			pr.Out.URL.Host = target

			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		// Stream SSE responses as they arrive.
		FlushInterval: -1,
	}, nil
}

// authority is hostport as a Host header carries it: without port when that is
// the scheme's default, which is how a client writes the authority of a URL
// that has no explicit port. A hostport that does not split is returned as is.
func authority(hostport, defaultPort string) string {
	host, port, err := net.SplitHostPort(hostport)

	if err != nil || port != defaultPort {
		return hostport
	}

	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}

	return host
}
