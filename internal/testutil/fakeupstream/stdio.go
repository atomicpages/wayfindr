package fakeupstream

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// childEnv names the environment variable that turns a re-executed test
// binary into a stdio fake. Its value is the JSON-encoded config.
const childEnv = "WAYFINDR_FAKEUPSTREAM_CONFIG"

// StdioCommand returns a command that runs a stdio fake pinned to version.
// The command re-executes the current test binary, so the calling package's
// TestMain must call [ServeStdioIfChild] (see the package doc).
//
// The command is not started. Hand it to an [mcp.CommandTransport], or copy
// its Path, Args, and the WAYFINDR_FAKEUPSTREAM_CONFIG entry of Env into a
// connector config.
func StdioCommand(t testing.TB, version string, opts ...Option) *exec.Cmd {
	t.Helper()
	cfg := mustConfig(t, version, opts)
	// Build the server once here so a bad option (for example a tool without
	// an object schema) fails the test instead of crashing the child.
	newServer(t.Context(), cfg)
	data, err := json.Marshal(cfg)

	if err != nil {
		t.Fatalf("fakeupstream: encode config: %v", err)
	}

	// -test.run=^$ keeps a child whose TestMain is not wired from running
	// the package's tests (and recursively spawning more children). The
	// test's context kills a child that outlives the test.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$") //nolint:gosec // re-exec of the test binary itself.
	cmd.Env = append(os.Environ(), childEnv+"="+string(data))

	return cmd
}

// ServeStdioIfChild serves a stdio fake and exits if this process was started
// by [StdioCommand]. Otherwise it returns at once. Call it first in TestMain.
func ServeStdioIfChild() {
	data, ok := os.LookupEnv(childEnv)

	if !ok {
		return
	}

	if err := serveStdio(context.Background(), data); err != nil {
		fmt.Fprintf(os.Stderr, "fakeupstream: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func serveStdio(ctx context.Context, data string) error {
	var cfg config

	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}

	if err := newServer(ctx, cfg).Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("serve stdio: %w", err)
	}

	return nil
}

// Serve runs a fake pinned to version on transport until the client
// disconnects or ctx ends. It is for programs outside `go test`, such as the
// stdio upstream `make conformance` configures; tests use [StdioCommand] or
// the HTTP starters instead.
func Serve(ctx context.Context, version string, transport mcp.Transport, opts ...Option) error {
	cfg, err := newConfig(version, opts)

	if err != nil {
		return err
	}

	if err := newServer(ctx, cfg).Run(ctx, transport); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}
