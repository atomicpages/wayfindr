package daemon

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/auth"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/gateway"
	"github.com/atomicpages/wayfindr/internal/httpclient"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// warnCheck is how often the daemon looks for new warnings to log. A check
// only reads: refresh stays lazy (ADR 0004).
//
// Warnings are judged by now, the clock the manager stamps states with and
// the broker reads token expiry by, not by the Clock that times the check.
const warnCheck = 15 * time.Minute

// health reads the warnings of the configured connectors (GLOSSARY:
// Warning): auth's for each credential, read against the connector in the
// running config, the client certificate's of a url connector with one
// (certificate_expiring), and upstream's for a connector stuck out of
// ready. The
// domains write the advice; health gathers it in one place (of) for every
// reader: the admin status (as admin.Health), the gateway (urgent), and
// the log (check).
type health struct {
	broker *auth.Broker
	lookup func(name string) (config.Connector, bool)
	status func() []upstream.ConnectorStatus
	now    func() time.Time
	clock  Clock
	log    zerolog.Logger
}

// warning is one connector's warning, from either domain.
type warning struct {
	connector, code string
	// note is a warning with nothing urgent yet: status shows it, the log
	// and the agent do not.
	note bool
	// urgent is a credential failure due within a day: the agent is told.
	urgent bool
	advice upstream.Advice
}

// warningKey is what makes two checks' warnings the same one.
type warningKey struct{ connector, code string }

// credential reads connector name's credential as of now: its health and
// auth's warnings. Both are zero for a connector the running config no
// longer names: its token is never sent.
func (h health) credential(ctx context.Context, name string, now time.Time) (auth.Health, []auth.Warning, error) {
	conn, ok := h.lookup(name)

	if !ok {
		return auth.Health{}, nil, nil
	}

	ch, err := h.broker.Health(ctx, conn)

	if err != nil {
		return auth.Health{}, nil, err
	}

	return ch, ch.Warnings(now, name, h.broker.Link(name)), nil
}

// of returns s's connector's credential health and every warning it has as
// of now: the credential's, the client certificate's, then stuck. When the
// credential cannot be read it returns the others, if any, and the error.
func (h health) of(ctx context.Context, s upstream.ConnectorStatus, now time.Time) (auth.Health, []warning, error) {
	ch, ws, err := h.credential(ctx, s.Connector, now)
	var out []warning

	for _, w := range ws {
		out = append(out, warning{
			connector: s.Connector, code: string(w.Code), note: w.Note, urgent: w.Urgent(now),
			advice: upstream.Advice{Summary: w.Summary, Action: w.Action, Link: w.Link},
		})
	}

	if w, ok := h.certificate(s.Connector, now); ok {
		out = append(out, w)
	}

	if a, ok := s.Stuck(now); ok {
		out = append(out, warning{connector: s.Connector, code: upstream.WarningStuck, advice: a})
	}

	return ch, out, err
}

// Health is admin.Health: the status row's token expiry and warnings.
func (h health) Health(ctx context.Context, s upstream.ConnectorStatus) (admin.ConnectorHealth, error) {
	ch, ws, err := h.of(ctx, s, h.now())

	if err != nil {
		return admin.ConnectorHealth{}, err
	}

	out := admin.ConnectorHealth{TokenExpiresAt: ch.AccessExpiry}

	for _, w := range ws {
		out.Warnings = append(out.Warnings, admin.Warning{
			Code: w.code, Note: w.note,
			Advice: admin.Advice{Summary: w.advice.Summary, Action: w.advice.Action, Link: w.advice.Link},
		})
	}

	return out, nil
}

// urgent is the gateway's Options.Warnings: the credential failures of
// connectors due within a day. Only their credentials are read; one that
// cannot be read is left out, and the next check logs why.
func (h health) urgent(ctx context.Context, connectors []string) []gateway.Warning {
	now := h.now()
	var out []gateway.Warning

	for _, name := range connectors {
		_, ws, _ := h.credential(ctx, name, now)

		for _, w := range ws {
			if w.Urgent(now) {
				out = append(out, gateway.Warning{Connector: name, Summary: w.Summary, Action: w.Action, Link: w.Link})
			}
		}

		if w, ok := h.certificate(name, now); ok && w.urgent {
			out = append(out, gateway.Warning{Connector: name, Summary: w.advice.Summary, Action: w.advice.Action})
		}
	}

	return out
}

// warningCertificateExpiring names the warning of a url connector whose
// client certificate ([connectors.<name>.tls] cert_file) expires within
// certWarnAhead, or has (GLOSSARY: Warning). It applies to any url
// connector with a client certificate, OAuth or not.
const warningCertificateExpiring = "certificate_expiring"

// certWarnAhead is how far ahead a client certificate's end is warned of,
// as a refresh token's is; within certWarnSoon it is urgent, and the agent
// is told.
const (
	certWarnAhead = 7 * 24 * time.Hour
	certWarnSoon  = 24 * time.Hour
)

// certificate returns the certificate_expiring warning of connector name,
// if the running config gives it a client certificate that expires within
// certWarnAhead of now, or has expired. The file is read at each check, so
// a rotated certificate clears the warning; one that cannot be read gives
// none here, since connecting fails and says why.
func (h health) certificate(name string, now time.Time) (warning, bool) {
	conn, ok := h.lookup(name)

	if !ok {
		return warning{}, false
	}

	at, err := httpclient.CertificateExpiry(conn)

	if err != nil || at.IsZero() || at.Sub(now) > certWarnAhead {
		return warning{}, false
	}

	verb, action := "expires", "Before then, replace cert_file and its key with a renewed certificate."

	if !now.Before(at) {
		verb, action = "expired", "Replace cert_file and its key with a renewed certificate."
	}

	return warning{
		connector: name, code: warningCertificateExpiring, urgent: at.Sub(now) <= certWarnSoon,
		advice: upstream.Advice{
			Summary: "The client certificate in [connectors." + name + ".tls] " + verb + " at " + at.UTC().Format(time.RFC3339) + ".",
			Action:  action + " Wayfindr presents it from the next connection, without a restart.",
		},
	}, true
}

// check reads every configured connector's warnings, logs at warn the ones
// that are new since logged (see remember), and returns what to remember
// for the next check. A credential that cannot be read is logged as an
// error.
func (h health) check(ctx context.Context, logged map[warningKey]bool) map[warningKey]bool {
	now := h.now()
	var found []warning
	unread := map[string]bool{}

	for _, s := range h.status() {
		_, ws, err := h.of(ctx, s, now)

		if err != nil {
			unread[s.Connector] = true
			h.log.Error().Err(err).Str("connector", s.Connector).Msg("read credential health")
		}

		found = append(found, ws...)
	}

	fresh, next := remember(logged, found, unread)

	for _, w := range fresh {
		h.log.Warn().Str("connector", w.connector).Str("warning", w.code).
			Str("summary", w.advice.Summary).Str("action", w.advice.Action).Msg("connector warning")
	}

	return next
}

// remember decides what a check logs: of found, the warnings (notes aside)
// that logged, the last check's set, lacks; and the set to remember next.
// A warning is logged once when it starts to apply, and again only once it
// has cleared and come back. A connector whose credential could not be
// read (unread) keeps the warnings logged for it: nothing shows they
// cleared, so the next good check does not log them again.
func remember(logged map[warningKey]bool, found []warning, unread map[string]bool) (fresh []warning, next map[warningKey]bool) {
	next = map[warningKey]bool{}

	for k := range logged {
		if unread[k.connector] {
			next[k] = true
		}
	}

	for _, w := range found {
		if w.note {
			continue
		}

		k := warningKey{w.connector, w.code}

		if !logged[k] && !next[k] {
			fresh = append(fresh, w)
		}

		next[k] = true
	}

	return fresh, next
}

// run checks for new warnings now and every warnCheck after, until ctx
// ends.
func (h health) run(ctx context.Context) {
	logged := h.check(ctx, nil)

	for {
		timer := h.clock.NewTimer(warnCheck)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C():
			logged = h.check(ctx, logged)
		}
	}
}

// watchHealth runs the warning check until ctx ends or stop is called;
// stop waits for it.
func (d *Daemon) watchHealth(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.health.run(ctx)
	}()

	return func() {
		cancel()
		<-done
	}
}
