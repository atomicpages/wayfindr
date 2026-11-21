//go:build !windows

package cli

import (
	"bytes"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// TestAuthInterruptedSaysHowToRetry: Ctrl-C while the command waits for
// the browser says how to retry, and exits non-zero.
func TestAuthInterruptedSaysHowToRetry(t *testing.T) {
	flows := &scriptedFlows{reports: []admin.FlowReport{{Connector: "linear", State: admin.FlowPending}}}
	host, _ := authHost(t, flows, nil)
	var stdout, stderr bytes.Buffer
	done := make(chan int)

	go func() { done <- RunWith([]string{"auth", "login", "linear"}, &stdout, &stderr, host) }()

	// The first long-poll comes after the command took over Ctrl-C, so the
	// signal can't end the test binary.
	for deadline := time.Now().Add(10 * time.Second); flows.waitCount() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the command never waited for the flow")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	var code int

	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not end after Ctrl-C")
	}

	if code == 0 {
		t.Error("exit code = 0, want non-zero")
	}

	want := authPrompt + "\nlinear was not authorized. Fix the cause, then run `wayfindr auth login linear`.\n"

	if diff := cmp.Diff(want, stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff("Error: the authorization of linear was interrupted\n", stderr.String()); diff != "" {
		t.Errorf("stderr (-want +got):\n%s", diff)
	}
}
