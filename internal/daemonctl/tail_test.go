package daemonctl_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/daemonctl"
)

func TestTailPrintsTheLastLines(t *testing.T) {
	long := strings.Repeat("x", 40<<10) // two of these cross a read chunk

	tests := []struct {
		name    string
		content string
		lines   int
		want    string
	}{
		{"all", "a\nb\nc\n", -1, "a\nb\nc\n"},
		{"last two", "a\nb\nc\n", 2, "b\nc\n"},
		{"none", "a\nb\nc\n", 0, ""},
		{"more than there are", "a\nb\nc\n", 5, "a\nb\nc\n"},
		{"unterminated last line", "a\nb\nc", 1, "c"},
		{"empty file", "", 3, ""},
		{"across read chunks", "first\n" + long + "\n" + long + "\nlast\n", 3, long + "\n" + long + "\nlast\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeLog(t, tt.content)
			var out bytes.Buffer

			if err := daemonctl.Tail(t.Context(), path, &out, tt.lines, false); err != nil {
				t.Fatalf("Tail: %v", err)
			}

			if diff := cmp.Diff(tt.want, out.String()); diff != "" {
				t.Errorf("output (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTailWithoutALogIsNotExist(t *testing.T) {
	err := daemonctl.Tail(t.Context(), filepath.Join(t.TempDir(), "wayfindr.log"), &bytes.Buffer{}, -1, false)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestTailFollowPrintsAppendedLinesAndStartsOverAfterATruncation(t *testing.T) {
	path := writeLog(t, "old\n")
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- daemonctl.Tail(ctx, path, out, 1, true) }()

	waitFor(t, out, "old\n")
	appendLog(t, path, "new\n")
	waitFor(t, out, "old\nnew\n")

	// Truncated and rewritten, as a log the daemon starts afresh would be.
	if err := os.WriteFile(path, []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	waitFor(t, out, "old\nnew\nfresh\n")
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Tail after cancel: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Tail did not return after its context ended")
	}
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wayfindr.log")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func appendLog(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // inside the test's temp dir

	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// waitFor waits until out holds exactly want.
func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for out.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer that Tail writes to while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
