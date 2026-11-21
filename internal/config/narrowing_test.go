package config_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/config"
)

// The token is ANDed with the profile and the file: a connector passes
// only if all three allow it, and a nil list allows every connector.
func TestNarrowingAllowsANDsTheTokenWithTheProfileAndTheFile(t *testing.T) {
	tests := []struct {
		name string
		n    config.Narrowing
		want map[string]bool
	}{
		{name: "zero narrows nothing", n: config.Narrowing{},
			want: map[string]bool{"github": true, "gmail": true}},
		{name: "token alone", n: config.Narrowing{Token: []string{"github"}},
			want: map[string]bool{"github": true, "gmail": false}},
		{name: "empty token allows none", n: config.Narrowing{Token: []string{}},
			want: map[string]bool{"github": false, "gmail": false}},
		{
			name: "a profile can't widen the token",
			n:    config.Narrowing{Token: []string{"github"}, Profile: &config.Scope{Connectors: []string{"github", "gmail"}}},
			want: map[string]bool{"github": true, "gmail": false},
		},
		{
			name: "a file can't widen the token",
			n:    config.Narrowing{Token: []string{"github"}, File: &config.Scope{Connectors: []string{"gmail"}}},
			want: map[string]bool{"github": false, "gmail": false},
		},
		{
			name: "all three",
			n: config.Narrowing{
				Token:   []string{"github", "gmail", "linear"},
				Profile: &config.Scope{Connectors: []string{"github", "linear"}},
				File:    &config.Scope{Connectors: []string{"linear", "gmail"}},
			},
			want: map[string]bool{"github": false, "gmail": false, "linear": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[string]bool{}

			for c := range tt.want {
				got[c] = tt.n.Allows(c)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Allows (-want +got):\n%s", diff)
			}
		})
	}
}

// Allowlist intersects the three lists in the profile's order, else the
// token's, else the file's. Nil is every connector and a non-nil empty
// list is none, which the store would otherwise read as every connector.
func TestNarrowingAllowlistIntersectsTheTokenInTheProfilesOrder(t *testing.T) {
	tests := []struct {
		name string
		n    config.Narrowing
		want []string
	}{
		{name: "nothing is every connector", n: config.Narrowing{}, want: nil},
		{name: "token alone", n: config.Narrowing{Token: []string{"gmail", "github"}}, want: []string{"gmail", "github"}},
		{name: "empty token is none", n: config.Narrowing{Token: []string{}}, want: []string{}},
		{
			name: "profile's order over the token's",
			n: config.Narrowing{
				Token:   []string{"linear", "github", "gmail"},
				Profile: &config.Scope{Connectors: []string{"github", "slack", "linear"}},
			},
			want: []string{"github", "linear"},
		},
		{
			name: "token's order over the file's",
			n: config.Narrowing{
				Token: []string{"linear", "github"},
				File:  &config.Scope{Connectors: []string{"github", "gmail", "linear"}},
			},
			want: []string{"linear", "github"},
		},
		{
			name: "all three",
			n: config.Narrowing{
				Token:   []string{"gmail", "linear", "github"},
				Profile: &config.Scope{Connectors: []string{"github", "linear", "slack"}},
				File:    &config.Scope{Connectors: []string{"linear", "github"}},
			},
			want: []string{"github", "linear"},
		},
		{
			name: "disjoint token and profile is none",
			n:    config.Narrowing{Token: []string{"github"}, Profile: &config.Scope{Connectors: []string{"gmail"}}},
			want: []string{},
		},
		{
			name: "a profile without an allowlist leaves the token's",
			n:    config.Narrowing{Token: []string{"github"}, Profile: &config.Scope{}},
			want: []string{"github"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.n.Allowlist()

			if (got == nil) != (tt.want == nil) {
				t.Fatalf("Allowlist() = %#v, want %#v: nil and empty differ", got, tt.want)
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Allowlist (-want +got):\n%s", diff)
			}
		})
	}
}

// Intersecting never edits the lists it was given.
func TestNarrowingAllowlistLeavesItsInputsAlone(t *testing.T) {
	profile := []string{"github", "gmail", "linear"}
	token := []string{"linear", "github"}
	n := config.Narrowing{Token: token, Profile: &config.Scope{Connectors: profile}}

	_ = n.Allowlist()

	if diff := cmp.Diff([]string{"github", "gmail", "linear"}, profile); diff != "" {
		t.Errorf("profile list changed (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"linear", "github"}, token); diff != "" {
		t.Errorf("token list changed (-want +got):\n%s", diff)
	}
}

// A token limits connectors, not tools: HidesName and Hides never name it,
// so a call outside the token can't be refused as hidden_by_*.
func TestNarrowingTokenHidesNoToolByName(t *testing.T) {
	n := config.Narrowing{Token: []string{"github"}}

	if got := n.HidesName("gmail", "send"); got != config.SourceNone {
		t.Errorf("HidesName(gmail, send) = %v, want SourceNone", got)
	}

	if got := n.Hides("gmail", "send", config.ClassDestructive); got != config.SourceNone {
		t.Errorf("Hides(gmail, send) = %v, want SourceNone", got)
	}
}
