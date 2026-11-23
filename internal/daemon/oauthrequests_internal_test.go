package daemon

import (
	"strings"
	"testing"
)

// suggestedBase is an unexported pure function that encodes a decision table
// from the design (design §7.11, daemon: "the first label of ClientHost,
// lowercased and cut to the connector-name rule; oauth if nothing is left"),
// so its table is the clearest spec. The approve route's seam, which
// oauthapprove_test.go drives, covers its effect.
func TestSuggestedBase(t *testing.T) {
	tests := []struct {
		host, want string
	}{
		{"chatgpt.com", "chatgpt"},
		{"ChatGPT.com", "chatgpt"},
		{"chatgpt.com:8443", "chatgpt"},
		{"www.example.com", "www"},
		{"my-app.example.com", "my-app"},
		{"my_app.example.com", "my_app"},
		{"localhost:8080", "localhost"},
		{"127.0.0.1:55443", "oauth"},
		{"[::1]:55443", "oauth"},
		{"::1", "oauth"},
		{"-lead.example.com", "lead"},
		{"trail-.example.com", "trail"},
		{"ünï.example.com", "n"},
		{"ü.example.com", "oauth"},
		{"", "oauth"},
		{".example.com", "oauth"},
		{strings.Repeat("a", 80) + ".example.com", strings.Repeat("a", 58)},
	}

	for _, tt := range tests {
		if got := suggestedBase(tt.host); got != tt.want {
			t.Errorf("suggestedBase(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}
