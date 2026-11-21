package auth_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

// pathSecret is the value of ${secret:zap}, which these tests put in a
// connector url's path, as per-user MCP urls do.
const pathSecret = "zap-k-123456"

func pathSecretMask() auth.DiscoverRequest {
	return auth.DiscoverRequest{Mask: config.Mask([]config.Resolved{
		{Ref: config.Ref{Kind: config.RefSecret, Name: "zap"}, Value: pathSecret},
	})}
}

// TestDiscoveryKeepsAURLSecretOutOfTheCacheAndLogs: the protected resource
// metadata of a url with a secret in its path names that url, so it is not
// cached; the authorization server's metadata, which doesn't, is
// (wayfindr-wt2v).
func TestDiscoveryKeepsAURLSecretOutOfTheCacheAndLogs(t *testing.T) {
	as := fakeas.Start(t)
	var resource string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource/s/"+pathSecret+"/mcp" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource, "authorization_servers": []string{as.Issuer}})
	}))
	t.Cleanup(srv.Close)
	resource = srv.URL + "/s/" + pathSecret + "/mcp"

	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	logs := &syncBuffer{}
	d := auth.NewDiscoverer(st, http.DefaultClient, zerolog.New(logs).Level(zerolog.DebugLevel))
	req := pathSecretMask()
	req.Connector, req.ResourceURL = "zap", resource
	req.Auth = config.Auth{Type: config.AuthTypeOAuth, PKCE: true, ValidateIssuer: true, ResourceIndicator: true}

	if _, err := d.Discover(t.Context(), req); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	prmURL := srv.URL + "/.well-known/oauth-protected-resource/s/" + pathSecret + "/mcp"

	if _, err := st.GetMetadata(t.Context(), prmURL); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetMetadata(PRM) error = %v, want not found: the document names the secret url", err)
	}

	if _, err := st.GetMetadata(t.Context(), as.MetadataURL); err != nil {
		t.Errorf("GetMetadata(AS metadata) error = %v, want it cached; logs:\n%s", err, logs)
	}

	if strings.Contains(logs.String(), pathSecret) {
		t.Errorf("logs hold the secret:\n%s", logs)
	}
}

// TestDiscoveryErrorMasksAURLSecret: a failure quotes the connector url,
// with the reference in place of the secret's value.
func TestDiscoveryErrorMasksAURLSecret(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	logs := &syncBuffer{}
	d := auth.NewDiscoverer(st, http.DefaultClient, zerolog.New(logs).Level(zerolog.DebugLevel))
	req := pathSecretMask()
	req.Connector, req.ResourceURL = "zap", down.URL+"/s/"+pathSecret+"/mcp"
	req.Auth = config.Auth{Type: config.AuthTypeOAuth, PKCE: true, ValidateIssuer: true}
	req.Probe = true

	_, err = d.Discover(t.Context(), req)

	if err == nil {
		t.Fatal("Discover succeeded, want an error from the unreachable server")
	}

	if strings.Contains(err.Error(), pathSecret) {
		t.Errorf("error = %q, want the secret masked", err)
	}

	if strings.Contains(logs.String(), pathSecret) {
		t.Errorf("logs hold the secret:\n%s", logs)
	}
}

// TestCredentialRowHoldsNoURL: the url a token is bound to is stored as its
// digest, so a secret in it never reaches the database.
func TestCredentialRowHoldsNoURL(t *testing.T) {
	h := newBrokerHarness(t)
	resource := h.as.URL + "/s/" + pathSecret + "/mcp"

	if err := auth.SeedToken(t.Context(), h.broker, "linear", resource, h.as.TokenEndpoint, "at", "rt",
		time.Now().Add(time.Hour), ""); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}

	if strings.Contains(cred.Resource, pathSecret) || cred.Resource != auth.ResourceKey(resource) {
		t.Errorf("stored resource = %q, want the url's digest", cred.Resource)
	}
}
