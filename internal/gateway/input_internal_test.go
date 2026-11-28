package gateway

import (
	"encoding/json"
	"testing"
)

// TestGatedArgumentsTable tests gatedArguments directly: it is a decision
// table of ADR 0024's rules on a gated call's arguments (CODING_STANDARDS.md,
// Tests), and one of its rules, nothing after the one JSON value, can't be
// reached through Call, whose schema check refuses such arguments first.
// TestGatedCallWithAmbiguousArgumentsIsRefused covers the rules' effect
// through Call.
func TestGatedArgumentsTable(t *testing.T) {
	tests := []struct {
		name, args string
		want       Case
	}{
		{"none", "", ""},
		{"an object", `{"to":"friend"}`, ""},
		{"an object, then whitespace", "{\"to\":\"friend\"} \n\t\r", ""},
		{"a second object after it", `{"a":1} {"b":2}`, CaseAmbiguousArguments},
		{"a number after it", `{"a":1}1`, CaseAmbiguousArguments},
		{"a stray bracket after it", `{"a":1}]`, CaseAmbiguousArguments},
		{"an unclosed object after it", `{"a":1}{`, CaseAmbiguousArguments},
		{"keys equal ignoring case", `{"to":1,"TO":2}`, CaseAmbiguousArguments},
		{"dotless i", "{\"file\":1,\"fıle\":2}", CaseAmbiguousArguments},
		{"dotted capital I", "{\"file\":1,\"fİle\":2}", CaseAmbiguousArguments},
		{"fi ligature", "{\"file\":1,\"ﬁle\":2}", CaseAmbiguousArguments},
		{"sharp s", "{\"ss\":1,\"ß\":2}", CaseAmbiguousArguments},
		{"full-width f", "{\"file\":1,\"ｆile\":2}", CaseAmbiguousArguments},
		{"NFC and NFD", "{\"café\":1,\"café\":2}", CaseAmbiguousArguments},
		{"Kelvin sign", "{\"k\":1,\"K\":2}", CaseAmbiguousArguments},
		{"distinct Cyrillic keys", "{\"Название\":1,\"Статус\":2}", ""},
		{"the same key in sibling objects", `{"a":[{"to":1},{"TO":2}]}`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gatedArguments(json.RawMessage(tt.args)); got != tt.want {
				t.Errorf("gatedArguments(%s) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
