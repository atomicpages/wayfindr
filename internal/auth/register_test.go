package auth_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
	"github.com/atomicpages/wayfindr/internal/testutil/fakeas"
)

const (
	defaultRedirect = "http://127.0.0.1:7777/oauth/callback"
	ngrokRedirect   = "https://example.ngrok.app/oauth/callback"
	cimdURL         = "https://me.example.com/wayfindr-client.json"
)

// testKey is a fixed master key; any 32 bytes do.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// regHarness is a Registrar over a real SQLite store and Sealer, talking to
// a fakeas server.
type regHarness struct {
	as     *fakeas.Server
	store  *sqlite.Store
	sealer *secrets.Sealer
	user   string
	r      *auth.Registrar
	d      *auth.Discoverer
}

func newRegHarness(t *testing.T, secretValues map[string]string, opts ...fakeas.Option) *regHarness {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "wayfindr.db"))

	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })
	user, err := st.EnsureUser(t.Context())

	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}

	sealer, err := secrets.NewSealer(testKey)

	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}

	resolver := config.ResolverFunc(func(_ context.Context, ref config.Ref) (string, error) {
		if v, ok := secretValues[ref.Name]; ok && ref.Kind == config.RefSecret {
			return v, nil
		}

		return "", fmt.Errorf("%s: %w", ref, config.ErrNotFound)
	})

	return &regHarness{
		as:     fakeas.Start(t, opts...),
		store:  st,
		sealer: sealer,
		user:   user,
		r:      auth.NewRegistrar(st, sealer, user, resolver, http.DefaultClient, zerolog.Nop()),
		d:      auth.NewDiscoverer(st, http.DefaultClient, zerolog.Nop()),
	}
}

// register discovers the fake's endpoints and registers connector "linear"
// with a and redirectURI.
func (h *regHarness) register(t *testing.T, a config.Auth, redirectURI string) (auth.Registration, error) {
	t.Helper()

	return h.registerWith(t, h.discover(t, a), a, redirectURI)
}

func (h *regHarness) discover(t *testing.T, a config.Auth) auth.Discovery {
	t.Helper()
	disc, err := h.d.Discover(t.Context(), auth.DiscoverRequest{Connector: "linear", ResourceURL: h.as.ResourceURL, Auth: a})

	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	return disc
}

func (h *regHarness) registerWith(t *testing.T, disc auth.Discovery, a config.Auth, redirectURI string) (auth.Registration, error) {
	t.Helper()

	return h.r.Register(t.Context(), auth.RegisterRequest{
		Connector:   "linear",
		Auth:        a,
		Discovery:   disc,
		RedirectURI: redirectURI,
	})
}

func TestRegisterUsesConfigClientIDFirst(t *testing.T) {
	h := newRegHarness(t, map[string]string{"linear_secret": "s3cret"}, fakeas.WithClientIDMetadataDocuments())
	a := defaultAuth()
	a.ClientID = "my-app"
	a.ClientSecret = "${secret:linear_secret}"
	a.ClientMetadataURL = cimdURL

	got, err := h.register(t, a, defaultRedirect)

	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	want := auth.Registration{ClientID: "my-app", ClientSecret: "s3cret", Method: auth.MethodConfig}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Register (-want +got):\n%s", diff)
	}

	if n := len(h.as.Clients()); n != 0 {
		t.Errorf("fakeas has %d registered clients, want 0 (no DCR)", n)
	}
}

func TestRegisterConfigSecretMissingIsAuthError(t *testing.T) {
	h := newRegHarness(t, nil)
	a := defaultAuth()
	a.ClientID = "my-app"
	a.ClientSecret = "${secret:linear_secret}"

	_, err := h.register(t, a, defaultRedirect)

	if e, ok := auth.AsError(err); !ok || e.Case != auth.CaseUnresolvedValue {
		t.Fatalf("Register error = %v, want case unresolved_value", err)
	}

	if !strings.Contains(err.Error(), "${secret:linear_secret}") {
		t.Errorf("Register error %q does not name the reference", err)
	}
}

func TestRegisterUsesCIMDWhenAdvertised(t *testing.T) {
	h := newRegHarness(t, nil, fakeas.WithClientIDMetadataDocuments())
	a := defaultAuth()
	a.ClientMetadataURL = cimdURL

	got, err := h.register(t, a, defaultRedirect)

	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	want := auth.Registration{ClientID: cimdURL, Method: auth.MethodCIMD}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Register (-want +got):\n%s", diff)
	}

	if n := len(h.as.Clients()); n != 0 {
		t.Errorf("fakeas has %d registered clients, want 0 (no DCR)", n)
	}
}

func TestRegisterFallsBackToDCRWithEffectiveRedirectURI(t *testing.T) {
	tests := []struct {
		name        string
		redirectURI string
		opts        []fakeas.Option
	}{
		{name: "default redirect, metadata endpoint", redirectURI: defaultRedirect},
		{name: "redirect_uri override", redirectURI: ngrokRedirect},
		{name: "legacy /register default", redirectURI: defaultRedirect, opts: []fakeas.Option{fakeas.WithoutMetadata()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRegHarness(t, nil, tt.opts...)
			a := defaultAuth()
			a.ClientMetadataURL = cimdURL // set, but the AS does not advertise CIMD

			got, err := h.register(t, a, tt.redirectURI)

			if err != nil {
				t.Fatalf("Register: %v", err)
			}

			clients := h.as.Clients()

			if len(clients) != 1 {
				t.Fatalf("fakeas has %d registered clients, want 1", len(clients))
			}

			if diff := cmp.Diff([]string{tt.redirectURI}, clients[0].RedirectURIs); diff != "" {
				t.Errorf("registered redirect_uris (-want +got):\n%s", diff)
			}

			want := auth.Registration{
				ClientID: clients[0].ID, ClientSecret: clients[0].Secret, Method: auth.MethodDCR,
				AuthMethod: config.AuthMethodClientSecretBasic,
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Register (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRegisterReusesStoredDCRClient(t *testing.T) {
	h := newRegHarness(t, nil)
	first, err := h.register(t, defaultAuth(), defaultRedirect)

	if err != nil {
		t.Fatalf("first Register: %v", err)
	}

	got, err := h.register(t, defaultAuth(), defaultRedirect)

	if err != nil {
		t.Fatalf("second Register: %v", err)
	}

	want := auth.Registration{
		ClientID: first.ClientID, ClientSecret: first.ClientSecret, Method: auth.MethodStoredDCR,
		AuthMethod: config.AuthMethodClientSecretBasic,
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Register (-want +got):\n%s", diff)
	}

	if n := len(h.as.Clients()); n != 1 {
		t.Errorf("fakeas has %d registered clients, want 1 (no second DCR)", n)
	}
}

func TestRegisterStoresDCRClientSealedToItsRow(t *testing.T) {
	h := newRegHarness(t, nil)
	reg, err := h.register(t, defaultAuth(), defaultRedirect)

	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if reg.ClientSecret == "" {
		t.Fatal("fakeas issued no client_secret; the test needs one")
	}

	cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}

	got := struct{ Resource, Issuer, RedirectURI, DCRClientID string }{
		cred.Resource, cred.Issuer, cred.RedirectURI, cred.DCRClientID,
	}
	want := struct{ Resource, Issuer, RedirectURI, DCRClientID string }{
		auth.ResourceKey(h.as.ResourceURL), h.as.Issuer, defaultRedirect, reg.ClientID,
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("credentials row (-want +got):\n%s", diff)
	}

	if bytes.Contains(cred.DCRClient, []byte(reg.ClientSecret)) {
		t.Error("dcr_client holds the client_secret in plaintext")
	}

	plain, err := h.sealer.Open(cred.DCRClient, secrets.AAD("credentials", h.user, "linear", "dcr_client"))

	if err != nil {
		t.Fatalf("open dcr_client with its row AAD: %v", err)
	}

	if !bytes.Contains(plain, []byte(reg.ClientSecret)) {
		t.Errorf("opened dcr_client %s lacks the client_secret", plain)
	}

	if _, err := h.sealer.Open(cred.DCRClient, secrets.AAD("credentials", h.user, "github", "dcr_client")); !errors.Is(err, secrets.ErrOpen) {
		t.Errorf("open dcr_client with another row's AAD: err = %v, want ErrOpen", err)
	}
}

func TestRegisterReRegistersWhenStoredClientNoLongerMatches(t *testing.T) {
	tests := []struct {
		name         string
		redirectURI  string
		changeIssuer bool
	}{
		{name: "redirect_uri changed", redirectURI: ngrokRedirect},
		{name: "issuer changed", redirectURI: defaultRedirect, changeIssuer: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRegHarness(t, nil)
			old, err := h.register(t, defaultAuth(), defaultRedirect)

			if err != nil {
				t.Fatalf("first Register: %v", err)
			}

			// A token from the old client is in the row too.
			cred, err := h.store.GetCredential(t.Context(), h.user, "linear")

			if err != nil {
				t.Fatalf("GetCredential: %v", err)
			}

			cred.Token = []byte("sealed-token")

			if err := h.store.PutCredential(t.Context(), cred); err != nil {
				t.Fatalf("PutCredential: %v", err)
			}

			disc := h.discover(t, defaultAuth())

			if tt.changeIssuer {
				disc.Issuer += "/moved"
			}

			got, err := h.registerWith(t, disc, defaultAuth(), tt.redirectURI)

			if err != nil {
				t.Fatalf("second Register: %v", err)
			}

			clients := h.as.Clients()

			if len(clients) != 2 {
				t.Fatalf("fakeas has %d registered clients, want 2", len(clients))
			}

			newest := clients[1]

			if newest.ID == old.ClientID {
				t.Fatal("re-registration returned the old client_id")
			}

			if diff := cmp.Diff([]string{tt.redirectURI}, newest.RedirectURIs); diff != "" {
				t.Errorf("registered redirect_uris (-want +got):\n%s", diff)
			}

			want := auth.Registration{
				ClientID: newest.ID, ClientSecret: newest.Secret, Method: auth.MethodDCR,
				AuthMethod: config.AuthMethodClientSecretBasic,
			}

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Register (-want +got):\n%s", diff)
			}

			stored, err := h.store.GetCredential(t.Context(), h.user, "linear")

			if err != nil {
				t.Fatalf("GetCredential: %v", err)
			}

			gotRow := struct {
				Resource, Issuer, RedirectURI, DCRClientID, Token string
			}{stored.Resource, stored.Issuer, stored.RedirectURI, stored.DCRClientID, string(stored.Token)}
			wantRow := struct {
				Resource, Issuer, RedirectURI, DCRClientID, Token string
			}{auth.ResourceKey(h.as.ResourceURL), disc.Issuer, tt.redirectURI, newest.ID, "sealed-token"}

			if diff := cmp.Diff(wantRow, gotRow); diff != "" {
				t.Errorf("credentials row (-want +got):\n%s", diff)
			}

			again, err := h.registerWith(t, disc, defaultAuth(), tt.redirectURI)

			if err != nil {
				t.Fatalf("third Register: %v", err)
			}

			if again.Method != auth.MethodStoredDCR || again.ClientID != newest.ID {
				t.Errorf("third Register = %s %s, want the new client reused (%s %s)",
					again.Method, again.ClientID, auth.MethodStoredDCR, newest.ID)
			}
		})
	}
}

func TestRegisterWithNoWayToRegisterIsAuthError(t *testing.T) {
	const wantMsg = `auth config (no_registration) for connector "linear": no client_id is configured and the authorization server ` +
		"supports neither CIMD nor dynamic client registration"
	tests := []struct {
		name        string
		opts        []fakeas.Option
		metadataURL string
	}{
		{name: "no DCR advertised", opts: []fakeas.Option{fakeas.WithoutDCR()}},
		{name: "client_metadata_url set, CIMD not advertised", metadataURL: cimdURL,
			opts: []fakeas.Option{fakeas.WithoutDCR()}},
		{name: "CIMD advertised, no client_metadata_url",
			opts: []fakeas.Option{fakeas.WithoutDCR(), fakeas.WithClientIDMetadataDocuments()}},
		{name: "legacy server without /register",
			opts: []fakeas.Option{fakeas.WithoutMetadata(), fakeas.WithoutDCR()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRegHarness(t, nil, tt.opts...)
			a := defaultAuth()
			a.ClientMetadataURL = tt.metadataURL

			_, err := h.register(t, a, defaultRedirect)

			if e, ok := auth.AsError(err); !ok || e.Kind != auth.KindConfig {
				t.Fatalf("Register error = %v, want kind config", err)
			}

			if got := err.Error(); got != wantMsg {
				t.Errorf("Register error:\n got %q\nwant %q", got, wantMsg)
			}
		})
	}
}

func TestRegisterDCRFailures(t *testing.T) {
	rejecting := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	tests := []struct {
		name     string
		handler  http.HandlerFunc // nil: the server is down
		wantKind auth.Kind
		wantCase auth.Case
		wantMsg  string // with %s for the server URL; empty to skip
	}{
		{
			name:     "rejected registration is auth_error",
			handler:  rejecting(http.StatusBadRequest, `{"error":"invalid_redirect_uri","error_description":"no loopback"}`),
			wantKind: auth.KindConfig, wantCase: auth.CaseRegistrationRejected,
			wantMsg: `auth config (registration_rejected) for connector "linear": dynamic client registration at %s/register ` +
				"was rejected [provider code invalid_redirect_uri]",
		},
		{
			name:     "server error is not auth_error",
			handler:  rejecting(http.StatusServiceUnavailable, `{"client_secret":"leaked"}`),
			wantKind: auth.KindMalformed, wantCase: auth.CaseMalformedResponse,
		},
		{name: "unreachable server is not auth_error", wantKind: auth.KindUnreachable, wantCase: auth.CaseTransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRegHarness(t, nil)
			srv := httptest.NewServer(tt.handler)

			if tt.handler == nil {
				srv.Close()
			} else {
				t.Cleanup(srv.Close)
			}

			disc := auth.Discovery{
				Resource:  "https://mcp.example.com/mcp",
				Issuer:    srv.URL,
				Endpoints: auth.Endpoints{Authorization: srv.URL + "/authorize", Token: srv.URL + "/token", Registration: srv.URL + "/register"},
				Source:    auth.SourceMetadata,
			}

			_, err := h.registerWith(t, disc, defaultAuth(), defaultRedirect)

			if err == nil {
				t.Fatal("Register succeeded, want an error")
			}

			if e, ok := auth.AsError(err); !ok || e.Kind != tt.wantKind || e.Case != tt.wantCase {
				t.Errorf("Register error = %v, want %s/%s", err, tt.wantKind, tt.wantCase)
			}

			if strings.Contains(err.Error(), "leaked") {
				t.Errorf("Register error %q quotes the response body", err)
			}

			if tt.wantMsg != "" {
				if want := fmt.Sprintf(tt.wantMsg, srv.URL); err.Error() != want {
					t.Errorf("Register error:\n got %q\nwant %q", err, want)
				}
			}

			var ue *url.Error

			if tt.handler == nil && !errors.As(err, &ue) {
				t.Errorf("Register error %v does not wrap the transport failure", err)
			}

			if _, err := h.store.GetCredential(t.Context(), h.user, "linear"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetCredential after failed registration: err = %v, want ErrNotFound", err)
			}
		})
	}
}
