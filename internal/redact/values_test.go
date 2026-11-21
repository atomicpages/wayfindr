package redact_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/atomicpages/wayfindr/internal/redact"
)

func TestValues(t *testing.T) {
	v := redact.NewValues(
		"k-123/abc", "${secret:zap}",
		"hunter2", "${env:PW}",
		"abc", "${env:SHORT}", // too short to mask
	)
	tests := []struct {
		name, in, want string
	}{
		{"path", `Post "https://mcp.zapier.com/s/k-123/abc/mcp": dial tcp: refused`,
			`Post "https://mcp.zapier.com/s/${secret:zap}/mcp": dial tcp: refused`},
		{"path-escaped form", "https://x.example/s/k-123%2Fabc/mcp", "https://x.example/s/${secret:zap}/mcp"},
		{"several values", "hunter2 and k-123/abc", "${env:PW} and ${secret:zap}"},
		{"short values are left alone", "abc is not masked", "abc is not masked"},
		{"no value", "connection refused", "connection refused"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := v.Apply(tt.in); got != tt.want {
				t.Errorf("Apply(%q) = %q, want %q", tt.in, got, tt.want)
			}

			if got, want := v.Contains(tt.in), tt.in != tt.want; got != want {
				t.Errorf("Contains(%q) = %v, want %v", tt.in, got, want)
			}
		})
	}
}

func TestValuesZeroMasksNothing(t *testing.T) {
	var v redact.Values

	if got := v.Apply("hunter2"); got != "hunter2" {
		t.Errorf("Apply = %q, want the text unchanged", got)
	}

	if v.Contains("hunter2") {
		t.Error("Contains = true, want false")
	}

	err := errors.New("hunter2")

	if got := v.Err(err); got != err { //nolint:errorlint // the same error, not a wrap of it
		t.Errorf("Err = %v, want err itself", got)
	}
}

func TestValuesErrKeepsTheChain(t *testing.T) {
	v := redact.NewValues("hunter2", "${secret:pw}")
	err := v.Err(fmt.Errorf("get https://x.example/hunter2: %w", io.ErrUnexpectedEOF))

	if got, want := err.Error(), "get https://x.example/${secret:pw}: unexpected EOF"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Error("errors.Is lost the wrapped error")
	}

	if v.Err(nil) != nil {
		t.Error("Err(nil) != nil")
	}
}
