package clienttoken_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
)

func TestGenerate(t *testing.T) {
	a, err := clienttoken.Generate()
	if err != nil {
		t.Fatal(err)
	}

	b, err := clienttoken.Generate()
	if err != nil {
		t.Fatal(err)
	}

	if a == b {
		t.Fatal("two tokens are equal")
	}

	raw, ok := strings.CutPrefix(a, clienttoken.ScopedPrefix)
	if !ok {
		t.Fatalf("token %q lacks prefix %q", a, clienttoken.ScopedPrefix)
	}

	if n, err := base64.RawURLEncoding.DecodeString(raw); err != nil || len(n) != 32 {
		t.Fatalf("token body: %d bytes, err %v; want 32 bytes of base64url", len(n), err)
	}
}

// About one random body in 4,096 begins "s_", which after DefaultPrefix
// reads as a scoped token, and the daemon refuses a scoped value as the
// default token (wayfindr-kg0j). 100,000 draws hit such a body about 24
// times, so a GenerateDefault that keeps it fails here all but surely.
func TestGenerateDefaultNeverReadsAsScoped(t *testing.T) {
	for range 100_000 {
		v, err := clienttoken.GenerateDefault()
		if err != nil {
			t.Fatal(err)
		}

		raw, ok := strings.CutPrefix(v, clienttoken.DefaultPrefix)
		if !ok {
			t.Fatalf("token %q lacks prefix %q", v, clienttoken.DefaultPrefix)
		}

		if clienttoken.IsScoped(v) {
			t.Fatalf("default token %q reads as a scoped token", v)
		}

		if n, err := base64.RawURLEncoding.DecodeString(raw); err != nil || len(n) != 32 {
			t.Fatalf("token body: %d bytes, err %v; want 32 bytes of base64url", len(n), err)
		}
	}
}

func TestTokenInfoRoundTrip(t *testing.T) {
	for _, c := range []clienttoken.Client{
		clienttoken.Default(),
		{Name: "nightly", Connectors: []string{"github", "linear"}},
		{Name: "grok", Connectors: []string{"github"}, Remote: true},
	} {
		ti := c.TokenInfo()

		if ti.UserID != c.Name {
			t.Errorf("UserID = %q, want %q", ti.UserID, c.Name)
		}

		got, ok := clienttoken.FromTokenInfo(ti)
		if !ok {
			t.Fatalf("FromTokenInfo(%v) found no client", c)
		}

		if diff := cmp.Diff(c, got); diff != "" {
			t.Errorf("round trip (-want +got):\n%s", diff)
		}

		if got.Scoped() != (c.Connectors != nil) {
			t.Errorf("Scoped() = %v for %v", got.Scoped(), c)
		}
	}
}

func TestDefaultIsNeverRemote(t *testing.T) {
	if clienttoken.Default().Remote {
		t.Error("Default().Remote = true, want false")
	}
}

func TestFromTokenInfoRefusesMissing(t *testing.T) {
	for _, ti := range []*auth.TokenInfo{nil, {UserID: "default"}, {Extra: map[string]any{"other": 1}}} {
		if _, ok := clienttoken.FromTokenInfo(ti); ok {
			t.Errorf("FromTokenInfo(%+v) found a client", ti)
		}
	}
}
