package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/oauthserver"
	"github.com/atomicpages/wayfindr/internal/store"
)

const (
	// defaultGrantName is the suggested grant name when the client's host
	// leaves nothing to build one from.
	defaultGrantName = "oauth"
	// maxSuggestedBase leaves room for freeName's "-99" in a name's 63 bytes.
	maxSuggestedBase = 58
)

// oauthRequests adapts the authorization server and the token service to
// admin.OAuthRequests (design §7.11, daemon): admin cannot import oauthserver,
// so the server's sentinels become the errors admin branches on.
type oauthRequests struct {
	server *oauthserver.Server
	tokens *clientTokens
}

var _ admin.OAuthRequests = oauthRequests{}

// Arm is Server.Arm.
func (o oauthRequests) Arm(context.Context) (time.Time, error) {
	return o.server.Arm(), nil
}

// Get claims the pending authorization (Server.Claim) and suggests a free
// grant name for its client. A reload, or the re-render after a refused
// approve, claims the same id again with no other effect.
func (o oauthRequests) Get(ctx context.Context, id string) (admin.OAuthRequest, error) {
	req, ok := o.server.Claim(id)

	if !ok {
		return admin.OAuthRequest{}, fmt.Errorf("pending authorization: %w", store.ErrNotFound)
	}

	name, err := o.tokens.freeName(ctx, suggestedBase(req.ClientHost))

	// Every suggestion taken is not a failure: the user types a name.
	if err != nil && !errors.Is(err, errNoFreeName) {
		return admin.OAuthRequest{}, err
	}

	return admin.OAuthRequest{
		ID:           req.ID,
		ClientID:     req.ClientID,
		ClientName:   req.ClientName,
		ClientHost:   req.ClientHost,
		RedirectHost: req.RedirectHost,
		StartedAt:    req.StartedAt,
		ReportedIP:   req.ReportedIP,
		Name:         name,
		ExpiresAt:    req.ExpiresAt,
		Arrived:      req.Arrived,
	}, nil
}

// Approve is Server.Approve, with oauthserver.ErrNoRequest as
// store.ErrNotFound, oauthserver.ErrInvalidName as admin.ErrInvalidApproval,
// and oauthserver.ErrContested as admin.ErrContested.
func (o oauthRequests) Approve(ctx context.Context, id, name string, connectors, approval []string) (string, error) {
	redirect, err := o.server.Approve(ctx, id, name, connectors, approval)

	switch {
	case errors.Is(err, oauthserver.ErrNoRequest):
		return "", fmt.Errorf("%w: %w", store.ErrNotFound, err)
	case errors.Is(err, oauthserver.ErrInvalidName):
		return "", fmt.Errorf("%w: %w", admin.ErrInvalidApproval, err)
	case errors.Is(err, oauthserver.ErrContested):
		return "", fmt.Errorf("%w: %w", admin.ErrContested, err)
	case err != nil:
		return "", err
	}

	return redirect, nil
}

// Deny is Server.Deny, with oauthserver.ErrNoRequest as store.ErrNotFound.
func (o oauthRequests) Deny(_ context.Context, id string) (string, error) {
	redirect, err := o.server.Deny(id)

	if errors.Is(err, oauthserver.ErrNoRequest) {
		return "", fmt.Errorf("%w: %w", store.ErrNotFound, err)
	}

	return redirect, err
}

// oauthChanged reports whether going from prev to next turns the
// authorization server on or off, changes its resource (public_url), or
// changes its allowed clients: what makes Reload disarm it.
func oauthChanged(prev, next config.Server) bool {
	scope := func(s config.Server) (string, []string) {
		if !s.OAuth.Enabled || s.Issuer() == "" {
			return "", nil
		}

		return oauthserver.Resource(s.Issuer()), s.OAuth.Clients
	}
	prevResource, prevClients := scope(prev)
	nextResource, nextClients := scope(next)

	return prevResource != nextResource || !slices.Equal(prevClients, nextClients)
}

// suggestedBase is the grant name to start from for a client at host (a
// host[:port]): the host's first label, lowercased and cut to the
// connector-name rule (chatgpt.com is chatgpt), or defaultGrantName when
// nothing is left or the host is an IP address.
func suggestedBase(host string) string {
	hostname, _, err := net.SplitHostPort(host)

	if err != nil {
		hostname = host
	}

	if _, err := netip.ParseAddr(strings.Trim(hostname, "[]")); err == nil {
		return defaultGrantName
	}

	label, _, _ := strings.Cut(hostname, ".")

	var b strings.Builder

	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			// Never first: a name starts with a letter or a digit.
			if b.Len() > 0 {
				b.WriteRune(r)
			}
		}
	}

	name := strings.TrimRight(b.String(), "-_")

	if len(name) > maxSuggestedBase {
		name = strings.TrimRight(name[:maxSuggestedBase], "-_")
	}

	if !config.IsConnectorName(name) {
		return defaultGrantName
	}

	return name
}
